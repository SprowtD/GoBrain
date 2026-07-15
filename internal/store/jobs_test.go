package store

import (
	"path/filepath"
	"testing"
)

func TestCreateJobDeduped(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	const photo = "data:image/jpeg;base64,AAAABBBBCCCC" // stand-in for the same shared photo

	// First upload: a real, new job.
	j1, dup, err := CreateJobDeduped("image", photo, "", "phone")
	if err != nil || dup {
		t.Fatalf("first upload: dup=%v err=%v; want new job", dup, err)
	}

	// Second and third uploads of the SAME photo collapse onto j1 — no new rows.
	for i := 0; i < 2; i++ {
		j, dup, err := CreateJobDeduped("image", photo, "", "phone")
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
	if _, dup, _ := CreateJobDeduped("image", photo+"XYZ", "", "phone"); dup {
		t.Fatal("different payload should not be a duplicate")
	}

	// Same payload but a different source_kind is distinct content.
	if _, dup, _ := CreateJobDeduped("thought", photo, "", "phone"); dup {
		t.Fatal("different source_kind should not collide")
	}
}

func TestCreateJobDeduped_RetryAfterFailure(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	const photo = "data:image/jpeg;base64,ZZZZ"

	j1, _, err := CreateJobDeduped("image", photo, "", "phone")
	if err != nil {
		t.Fatal(err)
	}
	// The first attempt fails (e.g. vision model error).
	if err := UpdateJobStatus(j1.ID, "misfiled", "vision model down"); err != nil {
		t.Fatal(err)
	}
	// Re-submitting the same photo must create a NEW job, not resurrect the failed one.
	j2, dup, err := CreateJobDeduped("image", photo, "", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if dup || j2.ID == j1.ID {
		t.Fatalf("after misfiled, re-upload should be a fresh job: dup=%v id=%s", dup, j2.ID)
	}
	// And now that a live job exists again, further uploads dedupe onto it.
	if _, dup, _ := CreateJobDeduped("image", photo, "", "phone"); !dup {
		t.Fatal("expected dedupe against the retried job")
	}
}

func TestCreateJob_AlwaysInserts(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	const p = "data:image/jpeg;base64,DUP"
	if _, err := CreateJob("image", p, "", "phone"); err != nil {
		t.Fatal(err)
	}
	// The force path (CreateJob) deliberately does not dedupe.
	if _, err := CreateJob("image", p, "", "phone"); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t); n != 2 {
		t.Fatalf("CreateJob rows = %d; want 2 (no dedupe on force path)", n)
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
