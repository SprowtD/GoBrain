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
	"encoding/base64"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/yuin/goldmark"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"

	"secondbrain-server/internal/index"
	"secondbrain-server/internal/store"
	"secondbrain-server/internal/vault"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.html"))

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

// maxImageUploadBytes caps a web-UI image upload. Enforced twice: as a body
// ceiling on the whole request (parseUpload) and again on the bytes actually
// read from the file field, so a mismatched Content-Length can't slip past it.
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

	job, duplicate, err := store.CreateJobDeduped(kind, payload, note, "web")
	if err != nil {
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}
	// An uploaded image's payload is a giant base64 data: URL — never worth
	// showing in the jobs list. Give it the filename as a display title
	// instead (only for the fresh job; a duplicate keeps whatever title the
	// original capture already has).
	if title != "" && !duplicate {
		_ = store.SetJobTitle(job.ID, title)
	}
	// Non-blocking enqueue: mirror the JSON handler's load-shedding. The row
	// stays 'queued' if the buffer is full. A duplicate capture is collapsed onto
	// the existing job, so there's nothing new to queue.
	if !duplicate {
		select {
		case h.queue <- job:
		default:
		}
	}

	h.renderJobs(w)
}

// parseUpload handles a multipart capture: the same payload/note/source_kind
// fields as a plain form post, plus an optional "file" field for an uploaded
// image. A file always wins the source kind (image) and becomes the payload
// as a base64 data: URL — the same format the mobile app already sends for
// shared photos, so it flows through the existing image pipeline (HEIC
// handling, vision call, vault asset storage) with no parallel code path. ok
// is false once parseUpload has already written an error response.
func (h *Handler) parseUpload(w http.ResponseWriter, r *http.Request) (payload, note, kind, title string, ok bool) {
	// A generous margin over the image cap covers the other form fields
	// (payload/note/kind, and multipart boundary overhead) without allowing a
	// second image-sized field to smuggle past the check below.
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
	if err := r.ParseMultipartForm(maxImageUploadBytes); err != nil {
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

	ctype := header.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ctype), "image/") {
		http.Error(w, "only image files are supported", http.StatusBadRequest)
		return "", "", "", "", false
	}

	data, err := io.ReadAll(io.LimitReader(file, maxImageUploadBytes+1))
	if err != nil {
		http.Error(w, "failed to read upload", http.StatusBadRequest)
		return "", "", "", "", false
	}
	if len(data) > maxImageUploadBytes {
		http.Error(w, "file too large (max 10MB)", http.StatusRequestEntityTooLarge)
		return "", "", "", "", false
	}

	payload = "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(data)
	return payload, note, "image", header.Filename, true
}

// Jobs renders the recent-jobs fragment (polled by the page).
func (h *Handler) Jobs(w http.ResponseWriter, _ *http.Request) {
	h.renderJobs(w)
}

// Retry re-queues a misfiled job's original capture with force semantics (the
// same content-hash dedup that normally collapses a re-submit onto the
// existing job is deliberately bypassed — that's the whole point of a retry).
// It mirrors the "force":true path of /v1/ingest, just scoped to one job's
// already-known kind/payload/note instead of taking a fresh request body.
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
	orig, err := store.GetJob(id)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	job, err := store.CreateJob(orig.SourceKind, orig.Payload, orig.Note, orig.TokenLabel)
	if err != nil {
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}
	select {
	case h.queue <- job:
	default:
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
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	content, err := vault.ReadNote(path)
	if err != nil {
		render(w, "note", noteData{Path: path, NotFound: true})
		return
	}
	frontmatter, body := splitFrontmatter(content)
	render(w, "note", noteData{Path: path, Frontmatter: frontmatter, Body: renderMarkdown(body)})
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

// splitFrontmatter separates a note's leading "---\n...\n---" YAML block (see
// ingest/render.go, which always emits exactly this shape) from its body. Text
// with no frontmatter block renders as-is.
func splitFrontmatter(text string) (frontmatter, body string) {
	if !strings.HasPrefix(text, "---\n") {
		return "", text
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", text
	}
	return strings.TrimSpace(rest[:end]), strings.TrimSpace(rest[end+4:])
}

// mdRenderer converts a note body to HTML. Raw HTML is deliberately left
// unsafe-disabled (the default: no html.WithUnsafe()) so any HTML that made it
// into scraped article/YouTube content is escaped rather than executed —
// notes are untrusted content, not templates.
var mdRenderer = goldmark.New(
	goldmark.WithRendererOptions(goldmarkhtml.WithHardWraps()),
)

func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(src), &buf); err != nil {
		log.Printf("web: render markdown: %v", err)
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>")
	}
	return template.HTML(buf.String())
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
