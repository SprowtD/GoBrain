package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"secondbrain-server/internal/store"
)

func NewRouter(jobQueue chan<- store.Job, backendURL string) *chi.Mux {
	r := chi.NewRouter()

	// Unauthenticated liveness probe for Railway health checks.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// All app-facing routes live under /v1 so the contract can evolve without
	// breaking self-hosted backends that clients haven't updated yet.
	r.Route("/v1", func(r chi.Router) {
		r.Use(AuthMiddleware)

		// Any valid token (admin or member) can capture and read.
		r.Post("/ingest", IngestHandler(jobQueue))
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
