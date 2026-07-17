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
	// Title is an optional display label for the job row — clients sending an
	// image as a base64 data: URL should set it (e.g. the photo's filename) so
	// job listings never have to fall back to the raw payload.
	Title string `json:"title,omitempty"`
	// Force re-ingests identical content instead of collapsing it onto the
	// existing job (e.g. deliberately re-capturing an article that changed).
	Force bool `json:"force,omitempty"`
}

type ingestResponse struct {
	JobID string `json:"job_id"`
	// Duplicate is true when this content was already captured, so JobID points
	// at the pre-existing job and nothing new was queued. OutputPath is that
	// job's note path when it has already filed.
	Duplicate  bool   `json:"duplicate,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
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

		job, duplicate, err := createJob(req, tokenLabel)
		if err != nil {
			http.Error(w, "failed to create job", http.StatusInternalServerError)
			return
		}

		// Already captured: return the existing job and don't re-queue, so the
		// same photo can't be filed twice. 200 (not 202) — nothing new started.
		if duplicate {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(ingestResponse{JobID: job.ID, Duplicate: true, OutputPath: job.OutputPath})
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

// createJob applies de-duplication unless the request opts out with force. It
// centralizes the choice so /ingest and /ingest/batch behave identically.
func createJob(req ingestRequest, tokenLabel string) (store.Job, bool, error) {
	title := truncate(req.Title, 200)
	if req.Force {
		job, err := store.CreateJob(req.SourceKind, req.Payload, req.Note, tokenLabel, title)
		return job, false, err
	}
	return store.CreateJobDeduped(req.SourceKind, req.Payload, req.Note, tokenLabel, title)
}

// truncate caps a client-supplied string at max runes (titles are display-only).
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
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
	Index     int    `json:"index"`
	JobID     string `json:"job_id,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"` // content already captured; JobID is the existing job
	Error     string `json:"error,omitempty"`
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

			job, duplicate, err := createJob(item, tokenLabel)
			if err != nil {
				results[i].Error = "failed to create job"
				continue
			}

			// Already captured (e.g. the same photo sent several times in one
			// batch): report the existing job and don't re-queue it.
			if duplicate {
				results[i].JobID = job.ID
				results[i].Duplicate = true
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
