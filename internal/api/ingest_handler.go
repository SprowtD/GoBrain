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

// maxBatchItems caps a single batch so one request can't flood the queue. The
// worker pool drains these concurrently; a bounded batch keeps the buffered
// queue (cap 128 in StartWorkers) from being blown by one caller.
const maxBatchItems = 50

type batchIngestRequest struct {
	Items []ingestRequest `json:"items"`
}

// batchItemResult carries the per-item outcome so a client that sent N photos
// can tell exactly which ones were queued and retry only the failures.
type batchItemResult struct {
	Index int    `json:"index"`
	JobID string `json:"job_id,omitempty"`
	Error string `json:"error,omitempty"`
}

type batchIngestResponse struct {
	Results []batchItemResult `json:"results"`
}

// BatchIngestHandler queues many captures in one request. Each item becomes an
// independent job, so the worker pool processes them in parallel exactly as if
// they'd been POSTed one by one — but the client gets a single round trip and a
// single auth. Partial success is normal: results carry a per-item error rather
// than failing the whole batch, so one bad photo doesn't sink the other nine.
func BatchIngestHandler(jobQueue chan<- store.Job) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req batchIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(req.Items) == 0 {
			http.Error(w, "items is required", http.StatusBadRequest)
			return
		}
		if len(req.Items) > maxBatchItems {
			http.Error(w, "too many items in batch", http.StatusBadRequest)
			return
		}

		tokenLabel, _ := r.Context().Value(tokenLabelKey).(string)

		results := make([]batchItemResult, len(req.Items))
		queued := 0
		for i, item := range req.Items {
			results[i].Index = i
			switch {
			case !isValidSourceKind(item.SourceKind):
				results[i].Error = "unknown source_kind"
				continue
			case item.Payload == "":
				results[i].Error = "payload is required"
				continue
			}

			job, err := store.CreateJob(item.SourceKind, item.Payload, item.Note, tokenLabel)
			if err != nil {
				results[i].Error = "failed to create job"
				continue
			}

			// Non-blocking enqueue, same load-shedding contract as /ingest: if
			// the queue is full the job row stays 'queued' for a later retry and
			// the item reports busy rather than hanging the whole batch.
			select {
			case jobQueue <- job:
				results[i].JobID = job.ID
				queued++
			default:
				results[i].Error = "server busy, retry shortly"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		// 202 if anything made it onto the queue; 503 only if every item was shed
		// for capacity (validation failures still return 202 with per-item errors).
		if queued == 0 && !anyQueuedOrValidation(results) {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
		json.NewEncoder(w).Encode(batchIngestResponse{Results: results})
	}
}

// anyQueuedOrValidation reports whether at least one item either queued or
// failed for a client-side reason (bad input) rather than server capacity — in
// which case the batch is a normal 202, not a 503 "come back later".
func anyQueuedOrValidation(results []batchItemResult) bool {
	for _, r := range results {
		if r.JobID != "" || (r.Error != "" && r.Error != "server busy, retry shortly") {
			return true
		}
	}
	return false
}

func isValidSourceKind(kind string) bool {
	switch kind {
	case "youtube", "article", "image", "thought":
		return true
	}
	return false
}
