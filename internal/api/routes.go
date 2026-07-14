package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"secondbrain-server/internal/store"
	"secondbrain-server/internal/web"
)

func NewRouter(jobQueue chan<- store.Job, backendURL string) *chi.Mux {
	r := chi.NewRouter()

	// Unauthenticated liveness probe for Railway health checks.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// htmx browser UI, served by this same process (no new service/cost).
	// The page shell and static assets are public so a browser can load them
	// before a token exists; the /ui/* data routes reuse AuthMiddleware, so the
	// token the page holds in localStorage gates them exactly like /v1.
	webUI := web.New(jobQueue)
	r.Get("/", webUI.Page)
	r.Handle("/static/*", web.StaticHandler())
	r.Route("/ui", func(r chi.Router) {
		r.Use(AuthMiddleware)
		r.Post("/ingest", webUI.Ingest)
		r.Get("/jobs", webUI.Jobs)
		r.Get("/search", webUI.Search)
		r.Get("/note", webUI.Note)
	})

	// All app-facing routes live under /v1 so the contract can evolve without
	// breaking self-hosted backends that clients haven't updated yet.
	r.Route("/v1", func(r chi.Router) {
		r.Use(AuthMiddleware)

		// Any valid token (admin or member) can capture and read.
		r.Post("/ingest", IngestHandler(jobQueue))
		r.Post("/ingest/batch", BatchIngestHandler(jobQueue))
		r.Get("/status/{id}", GetJobStatusHandler)
		r.Get("/status", ListRecentJobsHandler)

		// Direct note read/write/search — the surface the MCP server wraps.
		r.Post("/notes", WriteNoteHandler)
		r.Get("/notes/*", ReadNoteHandler)
		r.Delete("/notes/*", DeleteNoteHandler)
		r.Get("/search", SearchNotesHandler)
		r.Get("/related", RelatedNotesHandler)

		// Token management is admin-only: mint invites, list, and revoke.
		r.Group(func(r chi.Router) {
			r.Use(RequireAdmin)
			r.Post("/tokens", CreateTokenHandler(backendURL))
			r.Get("/tokens", ListTokensHandler)
			r.Delete("/tokens/{id}", RevokeTokenHandler)
		})
	})

	return r
}
