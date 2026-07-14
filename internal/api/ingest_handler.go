package api

import (
	"encoding/json"
	"net/http"

	"secondbrain-server/internal/store"
)

type ingestRequest struct {
	SourceKind string `json:"source_kind"`
	Payload    string `json:"payload"`
	Note       string `json:"note,omitempty"`
}

type ingestResponse struct {
	JobID string `json:"job_id"`
}

func IngestHandler(jobQueue chan<- store.Job) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		if !isValidSourceKind(req.SourceKind) {
			http.Error(w, "unknown source_kind", http.StatusBadRequest)
			return
		}
		if req.Payload == "" {
			http.Error(w, "payload is required", http.StatusBadRequest)
			return
		}

		tokenLabel, _ := r.Context().Value(tokenLabelKey).(string)

		job, err := store.CreateJob(req.SourceKind, req.Payload, req.Note, tokenLabel)
		if err != nil {
			http.Error(w, "failed to create job", http.StatusInternalServerError)
			return
		}

		// Buffered queue (see StartWorkers). If it's full we shed load rather
		// than hang the request; the job row stays 'queued' for a later retry.
		select {
		case jobQueue <- job:
		default:
			http.Error(w, "server busy, retry shortly", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(ingestResponse{JobID: job.ID})
	}
}

func isValidSourceKind(kind string) bool {
	switch kind {
	case "youtube", "article", "image", "thought":
		return true
	}
	return false
}
