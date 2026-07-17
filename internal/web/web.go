// Package web serves a single-page, htmx-driven browser UI from the same Go
// process that serves the /v1 JSON API. It reuses the store / index / vault
// packages directly — no logic is duplicated from the JSON handlers — and adds
// no new service or cost: it is just extra routes on the existing server.
//
// It deliberately does NOT import internal/api (which mounts these routes),
// keeping the dependency one-way and cycle-free. Auth for the /ui/* data routes
// is applied by the caller (api.AuthMiddleware) when mounting; the page shell
// and static assets are public so a browser can load them before a token exists.
package web

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"secondbrain-server/internal/index"
	"secondbrain-server/internal/ingest"
	"secondbrain-server/internal/store"
	"secondbrain-server/internal/vault"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

var tmpl = template.Must(template.New("").
	Funcs(template.FuncMap{"displayPayload": displayPayload}).
	ParseFS(templatesFS, "templates/*.html"))

// displayPayload is the jobs-list fallback label when a job has no title. A
// base64 data: URL (an uploaded/shared image on any ingest path) must never
// render as a row's name, and even text payloads get capped to a list-sized
// prefix.
func displayPayload(p string) string {
	if strings.HasPrefix(p, "data:") {
		return "image capture"
	}
	if r := []rune(p); len(r) > 140 {
		return string(r[:140]) + "…"
	}
	return p
}

// Handler holds the dependencies the web UI needs. The job queue is the same
// buffered channel the JSON /v1/ingest handler feeds.
type Handler struct {
	queue chan<- store.Job
}

// New builds a web Handler bound to the shared ingest queue.
func New(queue chan<- store.Job) *Handler {
	return &Handler{queue: queue}
}

// StaticHandler serves the embedded static assets (htmx, css) under /static/.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("web: static sub fs: %v", err)
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// Page renders the full single-page shell. Public (no token required); the page
// itself gates the app behind a connect step in the browser.
func (h *Handler) Page(w http.ResponseWriter, _ *http.Request) {
	render(w, "page.html", nil)
}

// maxImageUploadBytes caps a web-UI image upload. MaxBytesReader byte-counts
// the whole request (it doesn't trust Content-Length), with a small margin for
// the other form fields; the per-file re-check below it exists only to
// attribute the failure to the file with a precise message.
const maxImageUploadBytes = 10 << 20 // 10 MB

// Ingest queues a capture job and returns the refreshed job list fragment so the
// new row appears immediately. source_kind "auto" (or empty) is detected from
// the payload, matching the mobile app's behaviour. Requests encoded as
// multipart/form-data (the composer's Attach flow) may carry a "file" field
// instead of — or alongside — the payload text.
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	var payload, note, kind, title string

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		p, n, k, t, ok := h.parseUpload(w, r)
		if !ok {
			return // parseUpload already wrote the error response
		}
		payload, note, kind, title = p, n, k, t
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		payload = strings.TrimSpace(r.FormValue("payload"))
		note = strings.TrimSpace(r.FormValue("note"))
		kind = r.FormValue("source_kind")
	}

	if payload == "" {
		http.Error(w, "payload or file is required", http.StatusBadRequest)
		return
	}
	if kind == "" || kind == "auto" {
		kind = detectKind(payload)
	}

	// The title (an upload's filename) rides the INSERT itself so no poll can
	// ever observe an untitled image job; a duplicate capture keeps whatever
	// title the original already has.
	job, duplicate, err := store.CreateJobDeduped(kind, payload, note, "web", title)
	if err != nil {
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}
	// Non-blocking enqueue: mirror the JSON handler's load-shedding, including
	// its 503 — a silently shed job would sit 'queued' forever (nothing rescans
	// the table). A duplicate capture is collapsed onto the existing job, so
	// there's nothing new to queue.
	if !duplicate {
		select {
		case h.queue <- job:
		default:
			_ = store.UpdateJobStatus(job.ID, "misfiled", "server busy — hit capture to retry")
			http.Error(w, "server busy, retry shortly", http.StatusServiceUnavailable)
			return
		}
	}

	h.renderJobs(w)
}

// parseUpload handles a multipart capture: the same payload/note/source_kind
// fields as a plain form post, plus an optional "file" field for an uploaded
// image. A file wins the source kind (image) and becomes the payload as a
// base64 data: URL — the same format the mobile app already sends for shared
// photos, so it flows through the existing image pipeline (HEIC handling,
// vision call, vault asset storage) with no parallel code path. Text typed
// alongside an attachment is folded into the note, never dropped. ok is false
// once parseUpload has already written an error response.
func (h *Handler) parseUpload(w http.ResponseWriter, r *http.Request) (payload, note, kind, title string, ok bool) {
	// MaxBytesReader byte-counts the body; the margin over the image cap covers
	// the other form fields and multipart boundary overhead. The small
	// ParseMultipartForm memory budget spills the file part to a temp file
	// instead of holding all 10MB in the form buffer.
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "upload too large (max 10MB) or invalid form", http.StatusRequestEntityTooLarge)
		return "", "", "", "", false
	}

	note = strings.TrimSpace(r.FormValue("note"))
	kind = r.FormValue("source_kind")
	payload = strings.TrimSpace(r.FormValue("payload"))

	file, header, ferr := r.FormFile("file")
	if ferr != nil {
		// No file attached — htmx sends multipart once the form has a file
		// input regardless of whether one was actually chosen, so this is the
		// common "just text" case, not an error.
		return payload, note, kind, "", true
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxImageUploadBytes+1))
	if err != nil {
		http.Error(w, "failed to read upload", http.StatusBadRequest)
		return "", "", "", "", false
	}
	if len(data) > maxImageUploadBytes {
		http.Error(w, "file too large (max 10MB)", http.StatusRequestEntityTooLarge)
		return "", "", "", "", false
	}

	// Sniff the type from the bytes (the declared header lies both ways: HEIC
	// arrives as application/octet-stream, and an "image/svg+xml" would only
	// fail later, asynchronously, in the vision pipeline).
	ctype, err := ingest.SniffImageContentType(data, header.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", "", "", "", false
	}

	// The composer allows typing a thought AND attaching an image; the image
	// becomes the payload, so fold the typed text into the note — losing what
	// the user typed is the one unforgivable failure for a capture tool.
	if payload != "" {
		if note == "" {
			note = payload
		} else {
			note = payload + " — " + note
		}
	}

	return ingest.EncodeImageDataURL(ctype, data), note, "image", header.Filename, true
}

// Jobs renders the recent-jobs fragment (polled by the page).
func (h *Handler) Jobs(w http.ResponseWriter, _ *http.Request) {
	h.renderJobs(w)
}

// Retry flips a misfiled job back to 'queued' and re-enqueues it — the SAME
// row, not a clone, so its title and content-hash survive, the misfiled row's
// retry button disappears on the next poll, and a double-click (or a crafted
// id for a job that isn't misfiled) is a 409 no-op instead of a duplicate
// note plus a duplicate paid model call.
func (h *Handler) Retry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	job, err := store.RequeueJob(id)
	if errors.Is(err, store.ErrNotRetryable) {
		http.Error(w, "already retried or filed", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "failed to requeue job", http.StatusInternalServerError)
		return
	}
	select {
	case h.queue <- job:
	default:
		// Queue full: put the row back the way we found it (nothing rescans
		// 'queued' rows, so leaving it queued would strand it) and say so.
		_ = store.UpdateJobStatus(job.ID, "misfiled", "server busy — retry shortly")
		http.Error(w, "server busy, retry shortly", http.StatusServiceUnavailable)
		return
	}

	h.renderJobs(w)
}

type jobsView struct {
	Jobs     []store.Job
	Filed    int
	Active   int // queued + reading (in flight)
	Misfiled int
}

func (h *Handler) renderJobs(w http.ResponseWriter) {
	jobs, err := store.ListRecentJobs(50)
	if err != nil {
		http.Error(w, "failed to list jobs", http.StatusInternalServerError)
		return
	}
	v := jobsView{Jobs: jobs}
	for _, j := range jobs {
		switch j.Status {
		case "filed":
			v.Filed++
		case "misfiled":
			v.Misfiled++
		default:
			v.Active++
		}
	}
	render(w, "jobs", v)
}

// Search renders semantic (or keyword-fallback) search results as a fragment.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := searchData{Query: q}
	if q != "" {
		hits, err := index.Search(r.Context(), q, 20)
		if err != nil {
			http.Error(w, "search failed", http.StatusInternalServerError)
			return
		}
		data.Hits = hits
	}
	render(w, "search", data)
}

// Note renders a single note in a read panel: the YAML frontmatter as a small
// collapsed mono block (provenance, not prose) and the body as sanitized HTML
// via goldmark, so scraped article/YouTube/image notes read like content
// instead of a raw-markdown dump.
func (h *Handler) Note(w http.ResponseWriter, r *http.Request) {
	notePath := strings.TrimSpace(r.URL.Query().Get("path"))
	if notePath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	content, err := vault.ReadNote(notePath)
	if err != nil {
		render(w, "note", noteData{Path: notePath, NotFound: true})
		return
	}
	frontmatter, body, _ := vault.SplitFrontmatter(content)
	render(w, "note", noteData{
		Path:        notePath,
		Frontmatter: strings.TrimSpace(frontmatter),
		Body:        renderMarkdown(notePath, strings.TrimSpace(body)),
	})
}

type searchData struct {
	Query string
	Hits  []vault.SearchHit
}

type noteData struct {
	Path        string
	Frontmatter string
	Body        template.HTML
	NotFound    bool
}

// mdRenderer converts a note body to HTML. Notes are untrusted content, not
// templates: raw HTML in scraped article/YouTube text is rendered as VISIBLE
// escaped text by escapeRawHTML below — goldmark's default would swallow it
// with an "<!-- raw HTML omitted -->" comment, silently hiding content, and
// html.WithUnsafe would execute it.
var mdRenderer = goldmark.New(
	goldmark.WithRendererOptions(
		goldmarkhtml.WithHardWraps(),
		renderer.WithNodeRenderers(util.Prioritized(escapeRawHTML{}, 500)),
	),
)

// escapeRawHTML renders raw-HTML nodes as escaped text so angle-bracket
// content stays visible (matching what the old <pre> view guaranteed).
type escapeRawHTML struct{}

func (escapeRawHTML) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, renderEscapedRawHTML)
	reg.Register(ast.KindHTMLBlock, renderEscapedHTMLBlock)
}

func renderEscapedRawHTML(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	n := node.(*ast.RawHTML)
	for i := 0; i < n.Segments.Len(); i++ {
		seg := n.Segments.At(i)
		goldmarkhtml.DefaultWriter.RawWrite(w, seg.Value(source)) // RawWrite escapes
	}
	return ast.WalkSkipChildren, nil
}

func renderEscapedHTMLBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.HTMLBlock)
	if entering {
		for i := 0; i < n.Lines().Len(); i++ {
			line := n.Lines().At(i)
			goldmarkhtml.DefaultWriter.RawWrite(w, line.Value(source))
		}
	} else if n.HasClosure() {
		goldmarkhtml.DefaultWriter.RawWrite(w, n.ClosureLine.Value(source))
	}
	return ast.WalkContinue, nil
}

// maxInlineImageBytes caps how much of a stored asset gets inlined into the
// note panel as a data: URI. Vision-pipeline assets are typically well under
// this; anything bigger degrades to a broken embed rather than a huge fragment.
const maxInlineImageBytes = 8 << 20

func renderMarkdown(notePath, src string) template.HTML {
	source := []byte(src)
	doc := mdRenderer.Parser().Parse(text.NewReader(source))
	inlineVaultImages(doc, source, path.Dir(notePath))
	var buf bytes.Buffer
	if err := mdRenderer.Renderer().Render(&buf, source, doc); err != nil {
		log.Printf("web: render markdown: %v", err)
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(buf.String())
}

// inlineVaultImages rewrites relative image embeds (ingest stores an image
// note's source picture next to the note and embeds it by bare filename, an
// Obsidian convention) into data: URIs. No HTTP route serves vault assets —
// and <img> requests wouldn't carry the bearer token anyway — so inlining is
// what makes the picture actually show in the note panel.
func inlineVaultImages(doc ast.Node, source []byte, noteDir string) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		img, ok := n.(*ast.Image)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		dest := string(img.Destination)
		if strings.Contains(dest, "://") || strings.HasPrefix(dest, "data:") || strings.HasPrefix(dest, "/") {
			return ast.WalkContinue, nil // absolute/remote/inline already
		}
		data, err := vault.ReadFile(path.Join(noteDir, dest))
		if err != nil || len(data) == 0 || len(data) > maxInlineImageBytes {
			return ast.WalkContinue, nil // leave the embed as-is
		}
		ctype, err := ingest.SniffImageContentType(data, "")
		if err != nil {
			return ast.WalkContinue, nil
		}
		img.Destination = []byte(ingest.EncodeImageDataURL(ctype, data))
		return ast.WalkContinue, nil
	})
}

// detectKind maps a raw capture payload to a source_kind, matching the app.
func detectKind(payload string) string {
	lower := strings.ToLower(strings.TrimSpace(payload))
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if strings.Contains(lower, "youtube.com") || strings.Contains(lower, "youtu.be") {
			return "youtube"
		}
		return "article"
	}
	return "thought"
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("web: render %s: %v", name, err)
	}
}
