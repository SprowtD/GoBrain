package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"secondbrain-server/internal/store"
)

func initStore(t *testing.T) {
	t.Helper()
	if err := store.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("store init: %v", err)
	}
}

func postBatch(t *testing.T, q chan store.Job, body batchIngestRequest) (*httptest.ResponseRecorder, batchIngestResponse) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/batch", bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	BatchIngestHandler(q)(rec, req)
	var resp batchIngestResponse
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	}
	return rec, resp
}

func TestBatchIngest_PartialSuccessAndBusy(t *testing.T) {
	initStore(t)
	q := make(chan store.Job, 2) // cap 2, nobody drains -> 3rd valid item is shed

	rec, resp := postBatch(t, q, batchIngestRequest{Items: []ingestRequest{
		{SourceKind: "thought", Payload: "one"},   // 0: queued
		{SourceKind: "bogus", Payload: "x"},        // 1: unknown source_kind
		{SourceKind: "image", Payload: ""},          // 2: payload required
		{SourceKind: "thought", Payload: "two"},    // 3: queued (fills cap 2)
		{SourceKind: "thought", Payload: "three"},  // 4: server busy
	}})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(resp.Results) != 5 {
		t.Fatalf("results len = %d, want 5", len(resp.Results))
	}
	if resp.Results[0].JobID == "" {
		t.Errorf("item0: want job_id, got error %q", resp.Results[0].Error)
	}
	if resp.Results[1].Error != "unknown source_kind" {
		t.Errorf("item1: err = %q", resp.Results[1].Error)
	}
	if resp.Results[2].Error != "payload is required" {
		t.Errorf("item2: err = %q", resp.Results[2].Error)
	}
	if resp.Results[3].JobID == "" {
		t.Errorf("item3: want job_id, got error %q", resp.Results[3].Error)
	}
	if resp.Results[4].Error != "server busy, retry shortly" {
		t.Errorf("item4: err = %q", resp.Results[4].Error)
	}
	for i, r := range resp.Results {
		if r.Index != i {
			t.Errorf("result %d: Index = %d", i, r.Index)
		}
	}
}

func TestBatchIngest_AllShed503(t *testing.T) {
	initStore(t)
	q := make(chan store.Job) // unbuffered, no receiver -> every enqueue sheds

	rec, resp := postBatch(t, q, batchIngestRequest{Items: []ingestRequest{
		{SourceKind: "thought", Payload: "a"},
		{SourceKind: "thought", Payload: "b"},
	}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	for _, r := range resp.Results {
		if r.Error != "server busy, retry shortly" {
			t.Errorf("want busy, got %q", r.Error)
		}
	}
}

func TestBatchIngest_EmptyAndTooMany(t *testing.T) {
	initStore(t)
	q := make(chan store.Job, 128)

	rec, _ := postBatch(t, q, batchIngestRequest{Items: nil})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty: status = %d, want 400", rec.Code)
	}

	big := make([]ingestRequest, maxBatchItems+1)
	for i := range big {
		big[i] = ingestRequest{SourceKind: "thought", Payload: "x"}
	}
	rec, _ = postBatch(t, q, batchIngestRequest{Items: big})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("too many: status = %d, want 400", rec.Code)
	}
}
