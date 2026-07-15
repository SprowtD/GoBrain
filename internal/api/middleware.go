package api

import (
	"context"
	"net/http"
	"strings"

	"secondbrain-server/internal/store"
)

type contextKey string

const (
	tokenLabelKey contextKey = "tokenLabel"
	tokenRoleKey  contextKey = "tokenRole"
)

func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		rawToken := strings.TrimPrefix(authHeader, "Bearer ")
		if rawToken == "" || rawToken == authHeader {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		label, role, ok := store.VerifyToken(rawToken)
		if !ok {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), tokenLabelKey, label)
		ctx = context.WithValue(ctx, tokenRoleKey, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// corsMiddleware opens the MCP + OAuth routes to cross-origin browser clients
// (e.g. a web-based MCP connector). The bearer token travels in the Authorization
// header, not a cookie, so a wildcard origin is safe. Preflight OPTIONS requests
// are answered here directly.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin gates a route to admin tokens. Must be used inside a group that
// already ran AuthMiddleware.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, _ := r.Context().Value(tokenRoleKey).(string)
		if role != store.RoleAdmin {
			http.Error(w, "admin token required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
