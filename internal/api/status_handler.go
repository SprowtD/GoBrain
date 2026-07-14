package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"secondbrain-server/internal/store"
)

type statusResponse struct {
	JobID      string `json:"job_id"`
	SourceKind string `json:"source_kind"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
}

func GetJobStatusHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	job, err := store.GetJob(id)
	if err != nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statusResponse{
		JobID:      job.ID,
		SourceKind: job.SourceKind,
		Status:     job.Status,
		Error:      job.Error,
		OutputPath: job.OutputPath,
	})
}

func ListRecentJobsHandler(w http.ResponseWriter, r *http.Request) {
	jobs, err := store.ListRecentJobs(50)
	if err != nil {
		http.Error(w, "failed to list jobs", http.StatusInternalServerError)
		return
	}

	resp := make([]statusResponse, len(jobs))
	for i, j := range jobs {
		resp[i] = statusResponse{JobID: j.ID, SourceKind: j.SourceKind, Status: j.Status}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
