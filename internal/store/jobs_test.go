package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateJobDeduped(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	const photo = "data:image/jpeg;base64,AAAABBBBCCCC" // stand-in for the same shared photo

	// First upload: a real, new job.
	j1, dup, err := CreateJobDeduped("image", photo, "", "phone", "")
	if err != nil || dup {
		t.Fatalf("first upload: dup=%v err=%v; want new job", dup, err)
	}

	// Second and third uploads of the SAME photo collapse onto j1 — no new rows.
	for i := 0; i < 2; i++ {
		j, dup, err := CreateJobDeduped("image", photo, "", "phone", "")
		if err != nil {
			t.Fatal(err)
		}
		if !dup || j.ID != j1.ID {
			t.Fatalf("re-upload %d: dup=%v id=%s; want duplicate of %s", i, dup, j.ID, j1.ID)
		}
	}
	if n := countJobs(t); n != 1 {
		t.Fatalf("job rows = %d; want 1 (duplicates must not insert)", n)
	}

	// A different photo is not a duplicate.
	if _, dup, _ := CreateJobDeduped("image", photo+"XYZ", "", "phone", ""); dup {
		t.Fatal("different payload should not be a duplicate")
	}

	// Same payload but a different source_kind is distinct content.
	if _, dup, _ := CreateJobDeduped("thought", photo, "", "phone", ""); dup {
		t.Fatal("different source_kind should not collide")
	}
}

func TestCreateJobDeduped_RetryAfterFailure(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	const photo = "data:image/jpeg;base64,ZZZZ"

	j1, _, err := CreateJobDeduped("image", photo, "", "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	// The first attempt fails (e.g. vision model error).
	if err := UpdateJobStatus(j1.ID, "misfiled", "vision model down"); err != nil {
		t.Fatal(err)
	}
	// Re-submitting the same photo must create a NEW job, not resurrect the failed one.
	j2, dup, err := CreateJobDeduped("image", photo, "", "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	if dup || j2.ID == j1.ID {
		t.Fatalf("after misfiled, re-upload should be a fresh job: dup=%v id=%s", dup, j2.ID)
	}
	// And now that a live job exists again, further uploads dedupe onto it.
	if _, dup, _ := CreateJobDeduped("image", photo, "", "phone", ""); !dup {
		t.Fatal("expected dedupe against the retried job")
	}
}

func TestCreateJob_AlwaysInserts(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	const p = "data:image/jpeg;base64,DUP"
	if _, err := CreateJob("image", p, "", "phone", ""); err != nil {
		t.Fatal(err)
	}
	// The force path (CreateJob) deliberately does not dedupe.
	if _, err := CreateJob("image", p, "", "phone", ""); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t); n != 2 {
		t.Fatalf("CreateJob rows = %d; want 2 (no dedupe on force path)", n)
	}
}

func TestRequeueJob(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	j, _, err := CreateJobDeduped("image", "data:image/png;base64,QQQQ", "ctx", "web", "IMG_1995.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateJobStatus(j.ID, "misfiled", "vision model down"); err != nil {
		t.Fatal(err)
	}

	// Requeue reuses the SAME row: status flips, error clears, title survives.
	rq, err := RequeueJob(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rq.ID != j.ID || rq.Status != "queued" || rq.Error != "" || rq.Title != "IMG_1995.png" {
		t.Fatalf("requeued = %+v; want same id, queued, no error, title kept", rq)
	}
	if n := countJobs(t); n != 1 {
		t.Fatalf("job rows = %d; want 1 (requeue must not clone)", n)
	}

	// A second requeue (double-click) matches nothing — no duplicate work.
	if _, err := RequeueJob(j.ID); err != ErrNotRetryable {
		t.Fatalf("second requeue err = %v; want ErrNotRetryable", err)
	}
	// Filed jobs are equally not retryable (crafted-id defense).
	if err := CompleteJob(j.ID, "image/note.md", "Note"); err != nil {
		t.Fatal(err)
	}
	if _, err := RequeueJob(j.ID); err != ErrNotRetryable {
		t.Fatalf("requeue of filed job err = %v; want ErrNotRetryable", err)
	}
	// Unknown ids too.
	if _, err := RequeueJob("nope"); err != ErrNotRetryable {
		t.Fatalf("requeue of unknown id err = %v; want ErrNotRetryable", err)
	}
}

func TestListRecentJobs_TruncatesPayload(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	long := "data:image/png;base64," + strings.Repeat("A", 5000)
	j, err := CreateJob("image", long, "", "web", "big.png")
	if err != nil {
		t.Fatal(err)
	}
	list, err := ListRecentJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(list[0].Payload) != 200 {
		t.Fatalf("list payload len = %d; want 200-char summary", len(list[0].Payload))
	}
	// The full payload is still there for the pipeline.
	full, err := GetJob(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Payload != long {
		t.Fatalf("GetJob payload truncated (len %d); want full", len(full.Payload))
	}
}

func countJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
