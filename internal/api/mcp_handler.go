package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"secondbrain-server/internal/index"
	"secondbrain-server/internal/mcp"
	"secondbrain-server/internal/note"
	"secondbrain-server/internal/store"
	"secondbrain-server/internal/vault"
)

// MCPHandler serves the Model Context Protocol over the Streamable HTTP
// transport at /mcp, so any MCP-capable agent adds the vault by URL — no local
// binary to build. It runs the shared mcp core (identical tools to the stdio
// server) against an in-process Backend that touches the vault directly.
//
// Auth accepts either an OAuth 2.1 access token (the "paste a URL" flow, see
// oauth.go) or a plain minted GoBrain token (the header one-liner). Both resolve
// to a (label, role) identity used for note attribution.
func MCPHandler(backendURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			serveMCPPost(w, r, backendURL)
		case http.MethodGet, http.MethodDelete:
			// We don't offer a server-initiated SSE stream or stateful sessions;
			// the spec allows replying 405 so clients fall back to POST-only.
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func serveMCPPost(w http.ResponseWriter, r *http.Request, backendURL string) {
	label, _, ok := authenticateMCP(r)
	if !ok {
		// Point unauthenticated clients at our protected-resource metadata so
		// they can discover the authorization server and run the OAuth flow.
		base := baseURL(r, backendURL)
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		http.Error(w, "authorization required", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req mcp.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	ctx := r.Context()
	result, rerr := mcp.Handle(ctx, req, inProcessBackend{author: label})

	if req.IsNotification() {
		// Notifications/responses carry no id and expect no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// authenticateMCP resolves the bearer token on an /mcp request to an identity,
// trying OAuth access tokens first, then plain minted tokens.
func authenticateMCP(r *http.Request) (label, role string, ok bool) {
	raw := bearerToken(r)
	if raw == "" {
		return "", "", false
	}
	if label, role, ok = store.VerifyOAuthAccessToken(raw); ok {
		return label, role, true
	}
	return store.VerifyToken(raw)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mcp.Response{JSONRPC: "2.0", ID: id, Error: &mcp.Error{Code: code, Message: msg}})
}

// --- in-process Backend: touches the vault/index/note packages directly ---

type inProcessBackend struct {
	author string // token label, for note attribution
}

func (inProcessBackend) Search(ctx context.Context, query string) ([]mcp.SearchHit, error) {
	hits, err := index.Search(ctx, query, 20)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.SearchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, mcp.SearchHit{Path: h.Path, Title: h.Title, Snippet: h.Snippet})
	}
	return out, nil
}

func (inProcessBackend) Read(_ context.Context, path string) (string, error) {
	content, err := vault.ReadNote(path)
	if errors.Is(err, vault.ErrNotFound) {
		return "", mcp.ErrNotFound
	}
	return content, err
}

func (inProcessBackend) Related(ctx context.Context, path string) ([]mcp.RelatedHit, error) {
	hits, err := index.Related(ctx, path, 5)
	if errors.Is(err, vault.ErrNotFound) {
		return nil, mcp.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make([]mcp.RelatedHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, mcp.RelatedHit{Path: h.Path, Title: h.Title})
	}
	return out, nil
}

func (b inProcessBackend) Write(_ context.Context, in mcp.WriteInput) (string, error) {
	n := note.Note{
		Kind: in.Kind, Title: in.Title, Tags: in.Tags,
		Body: in.Body, Project: in.Project, Author: b.author,
	}
	rel, content := n.Render(time.Now().UTC().Format(time.RFC3339))
	if err := vault.WriteSync(vault.WriteRequest{RelPath: rel, Content: content}); err != nil {
		return "", err
	}
	return rel, nil
}

func (inProcessBackend) Delete(_ context.Context, path string) error {
	err := vault.DeleteNote(path)
	if errors.Is(err, vault.ErrNotFound) {
		return mcp.ErrNotFound
	}
	return err
}
