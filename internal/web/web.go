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
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"

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

// Ingest queues a capture job and returns the refreshed job list fragment so the
// new row appears immediately. source_kind "auto" (or empty) is detected from
// the payload, matching the mobile app's behaviour.
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	payload := strings.TrimSpace(r.FormValue("payload"))
	if payload == "" {
		http.Error(w, "payload is required", http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	kind := r.FormValue("source_kind")
	if kind == "" || kind == "auto" {
		kind = detectKind(payload)
	}

	job, duplicate, err := store.CreateJobDeduped(kind, payload, note, "web")
	if err != nil {
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
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

// Jobs renders the recent-jobs fragment (polled by the page).
func (h *Handler) Jobs(w http.ResponseWriter, _ *http.Request) {
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

// Note renders a single note's raw markdown in a read panel.
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
	render(w, "note", noteData{Path: path, Content: content})
}

type searchData struct {
	Query string
	Hits  []vault.SearchHit
}

type noteData struct {
	Path     string
	Content  string
	NotFound bool
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
