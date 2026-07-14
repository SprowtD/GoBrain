package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"secondbrain-server/internal/index"
	"secondbrain-server/internal/note"
	"secondbrain-server/internal/vault"
)

type writeNoteRequest struct {
	Kind    string   `json:"kind,omitempty"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags,omitempty"`
	Body    string   `json:"body"`
	Project string   `json:"project,omitempty"`
}

type writeNoteResponse struct {
	Path string `json:"path"`
}

// WriteNoteHandler writes a pre-structured, un-chunked note straight into the
// vault (used by the MCP server / agents). Distinct from /ingest.
func WriteNoteHandler(w http.ResponseWriter, r *http.Request) {
	var req writeNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" || req.Body == "" {
		http.Error(w, "title and body are required", http.StatusBadRequest)
		return
	}

	author, _ := r.Context().Value(tokenLabelKey).(string)
	n := note.Note{
		Kind: req.Kind, Title: req.Title, Tags: req.Tags,
		Body: req.Body, Project: req.Project, Author: author,
	}
	rel, content := n.Render(time.Now().UTC().Format(time.RFC3339))

	if err := vault.WriteSync(vault.WriteRequest{RelPath: rel, Content: content}); err != nil {
		http.Error(w, "failed to write note", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(writeNoteResponse{Path: rel})
}

type readNoteResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func ReadNoteHandler(w http.ResponseWriter, r *http.Request) {
	rel := chi.URLParam(r, "*")
	content, err := vault.ReadNote(rel)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			http.Error(w, "note not found", http.StatusNotFound)
			return
		}
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(readNoteResponse{Path: rel, Content: content})
}

// DeleteNoteHandler removes a note by vault path. The vault is git-backed, so a
// mistaken delete is recoverable from history.
func DeleteNoteHandler(w http.ResponseWriter, r *http.Request) {
	rel := chi.URLParam(r, "*")
	if err := vault.DeleteNote(rel); err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			http.Error(w, "note not found", http.StatusNotFound)
			return
		}
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func SearchNotesHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}

	// Semantic search when embeddings are configured; keyword fallback otherwise.
	hits, err := index.Search(r.Context(), q, 20)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	if hits == nil {
		hits = []vault.SearchHit{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hits)
}

// RelatedNotesHandler returns notes semantically nearest to a given note path.
// Empty result (not an error) when semantic search is disabled.
func RelatedNotesHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	hits, err := index.Related(r.Context(), path, 5)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			http.Error(w, "note not found", http.StatusNotFound)
			return
		}
		http.Error(w, "related lookup failed", http.StatusInternalServerError)
		return
	}
	if hits == nil {
		hits = []vault.SearchHit{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hits)
}
