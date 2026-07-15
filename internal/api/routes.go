package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"secondbrain-server/internal/store"
	"secondbrain-server/internal/web"
)

func NewRouter(jobQueue chan<- store.Job, backendURL string) *chi.Mux {
	r := chi.NewRouter()

	// CORS is mounted at the root so it wraps the entire mux — including requests
	// that match no method route. That matters for OAuth: browser-based MCP
	// clients send an OPTIONS preflight to /oauth/token and /oauth/register, and
	// those paths only register POST. A group-scoped middleware wouldn't run for
	// the unmatched OPTIONS (chi answers it with a bare 405, no CORS headers), so
	// the preflight — and thus the token exchange — would fail. At the root, every
	// preflight is answered. Safe as a wildcard because auth is a bearer header,
	// never a cookie, so there's no ambient-credential surface to protect.
	r.Use(corsMiddleware)

	// Unauthenticated liveness probe for Railway health checks.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Remote MCP connector: the Streamable HTTP MCP endpoint plus its OAuth 2.1
	// authorization server, so any agent adds the vault by URL alone (no local
	// binary). All are public at the router level — /mcp authenticates itself
	// (OAuth access token or minted token), and the OAuth/discovery routes must
	// be reachable pre-auth. CORS (mounted at the root above) lets browser-based
	// connectors reach these.
	r.Group(func(r chi.Router) {
		// RFC 9728 / RFC 8414 discovery. Some clients probe the /mcp-suffixed
		// protected-resource path, so both are served.
		r.Get("/.well-known/oauth-protected-resource", ProtectedResourceMetadata(backendURL))
		r.Get("/.well-known/oauth-protected-resource/mcp", ProtectedResourceMetadata(backendURL))
		r.Get("/.well-known/oauth-authorization-server", AuthServerMetadata(backendURL))

		r.Post("/oauth/register", RegisterClientHandler)
		r.HandleFunc("/oauth/authorize", AuthorizeHandler) // GET renders consent, POST processes it
		r.Post("/oauth/token", TokenHandler)

		r.Handle("/mcp", MCPHandler(backendURL))
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
