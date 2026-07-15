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

func postIngest(t *testing.T, q chan store.Job, body ingestRequest) (*httptest.ResponseRecorder, ingestResponse) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	IngestHandler(q)(rec, req)
	var resp ingestResponse
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	}
	return rec, resp
}

func TestIngest_DeduplicatesRepeatUpload(t *testing.T) {
	initStore(t)
	q := make(chan store.Job, 128)
	const photo = "data:image/jpeg;base64,SAMEPHOTO"

	// First upload: queued (202), a new job.
	rec, first := postIngest(t, q, ingestRequest{SourceKind: "image", Payload: photo})
	if rec.Code != http.StatusAccepted || first.JobID == "" || first.Duplicate {
		t.Fatalf("first: code=%d job=%q dup=%v; want 202, a job, not dup", rec.Code, first.JobID, first.Duplicate)
	}

	// Second identical upload: 200, flagged duplicate, same job, NOT re-queued.
	rec, second := postIngest(t, q, ingestRequest{SourceKind: "image", Payload: photo})
	if rec.Code != http.StatusOK || !second.Duplicate || second.JobID != first.JobID {
		t.Fatalf("second: code=%d dup=%v job=%q; want 200, dup, same job %q", rec.Code, second.Duplicate, second.JobID, first.JobID)
	}
	if len(q) != 1 {
		t.Fatalf("queue depth = %d; want 1 (duplicate must not enqueue)", len(q))
	}

	// force re-ingests: a brand-new job, queued again.
	rec, forced := postIngest(t, q, ingestRequest{SourceKind: "image", Payload: photo, Force: true})
	if rec.Code != http.StatusAccepted || forced.Duplicate || forced.JobID == first.JobID {
		t.Fatalf("forced: code=%d dup=%v job=%q; want 202, fresh job", rec.Code, forced.Duplicate, forced.JobID)
	}
	if len(q) != 2 {
		t.Fatalf("queue depth = %d; want 2 after forced re-ingest", len(q))
	}
}

func TestBatchIngest_CollapsesRepeatsWithinBatch(t *testing.T) {
	initStore(t)
	q := make(chan store.Job, 128)
	const photo = "data:image/png;base64,DUPEDUPE"

	_, resp := postBatch(t, q, batchIngestRequest{Items: []ingestRequest{
		{SourceKind: "image", Payload: photo},   // 0: new
		{SourceKind: "image", Payload: photo},   // 1: duplicate of 0
		{SourceKind: "image", Payload: photo},   // 2: duplicate of 0
		{SourceKind: "thought", Payload: "note"}, // 3: new, distinct
	}})
	if resp.Results[0].JobID == "" || resp.Results[0].Duplicate {
		t.Fatalf("item0 should be a new job: %+v", resp.Results[0])
	}
	for _, i := range []int{1, 2} {
		if !resp.Results[i].Duplicate || resp.Results[i].JobID != resp.Results[0].JobID {
			t.Fatalf("item%d should duplicate item0: %+v", i, resp.Results[i])
		}
	}
	if resp.Results[3].JobID == "" || resp.Results[3].Duplicate {
		t.Fatalf("item3 (distinct) should be new: %+v", resp.Results[3])
	}
	// Only the two distinct captures were queued.
	if len(q) != 2 {
		t.Fatalf("queue depth = %d; want 2 (one photo + one thought)", len(q))
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
