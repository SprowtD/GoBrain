package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"secondbrain-server/internal/store"
)

type createTokenRequest struct {
	Label string `json:"label"`
	Role  string `json:"role,omitempty"` // "admin" or "member" (default member)
}

type createTokenResponse struct {
	Token    string `json:"token"`     // shown once — the raw secret
	Role     string `json:"role"`      // the granted role
	JoinLink string `json:"join_link"` // deep link for the mobile app
}

func CreateTokenHandler(backendURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
			http.Error(w, "label is required", http.StatusBadRequest)
			return
		}

		role := req.Role
		if role == "" {
			role = store.RoleMember
		}
		if role != store.RoleAdmin && role != store.RoleMember {
			http.Error(w, "role must be 'admin' or 'member'", http.StatusBadRequest)
			return
		}

		_, rawToken, err := store.CreateToken(req.Label, role)
		if err != nil {
			http.Error(w, "failed to create token", http.StatusInternalServerError)
			return
		}

		joinLink := fmt.Sprintf("secondbrain://join?url=%s&token=%s", backendURL, rawToken)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createTokenResponse{Token: rawToken, Role: role, JoinLink: joinLink})
	}
}

type tokenListItem struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Role      string `json:"role"`
	Revoked   bool   `json:"revoked"`
	CreatedAt string `json:"created_at"`
}

func ListTokensHandler(w http.ResponseWriter, r *http.Request) {
	tokens, err := store.ListTokens()
	if err != nil {
		http.Error(w, "failed to list tokens", http.StatusInternalServerError)
		return
	}

	resp := make([]tokenListItem, len(tokens))
	for i, t := range tokens {
		resp[i] = tokenListItem{ID: t.ID, Label: t.Label, Role: t.Role, Revoked: t.Revoked, CreatedAt: t.CreatedAt}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func RevokeTokenHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := store.RevokeToken(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "token not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to revoke token", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
