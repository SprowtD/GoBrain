package index

import (
	"math"
	"testing"

	"secondbrain-server/internal/store"
)

func TestNormalizeAndDotGiveCosine(t *testing.T) {
	a := []float32{3, 4} // length 5
	normalize(a)
	if d := dot(a, a); math.Abs(d-1) > 1e-6 {
		t.Errorf("unit vector dotted with itself = %v, want 1", d)
	}

	x := []float32{1, 0}
	y := []float32{0, 1}
	if d := dot(x, y); math.Abs(d) > 1e-6 {
		t.Errorf("orthogonal dot = %v, want 0", d)
	}

	// A zero vector must be left untouched (no NaN from divide-by-zero).
	z := []float32{0, 0}
	normalize(z)
	if z[0] != 0 || z[1] != 0 {
		t.Errorf("normalize(zero) = %v, want unchanged", z)
	}
}

func TestRankOrdersExcludesAndSkipsDimMismatch(t *testing.T) {
	q := []float32{1, 0}
	normalize(q)

	mk := func(v ...float32) []float32 { normalize(v); return v }
	embs := []store.Embedding{
		{RelPath: "a.md", Vec: mk(1, 0)},       // most similar
		{RelPath: "b.md", Vec: mk(0, 1)},       // orthogonal
		{RelPath: "c.md", Vec: mk(0.7, 0.7)},   // in between
		{RelPath: "wrong.md", Vec: []float32{1, 0, 0}}, // dim mismatch -> skipped
	}

	got := rank(q, embs, "", 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 ranked (dim mismatch skipped), got %d", len(got))
	}
	if got[0].path != "a.md" || got[1].path != "c.md" || got[2].path != "b.md" {
		t.Errorf("wrong order: %v", []string{got[0].path, got[1].path, got[2].path})
	}

	// Exclude the query note itself.
	excl := rank(q, embs, "a.md", 10)
	for _, s := range excl {
		if s.path == "a.md" {
			t.Fatal("excluded path leaked into results")
		}
	}
	if excl[0].path != "c.md" {
		t.Errorf("after exclude, top = %q, want c.md", excl[0].path)
	}

	// Limit is honored.
	if lim := rank(q, embs, "", 2); len(lim) != 2 {
		t.Errorf("limit not applied: got %d", len(lim))
	}
}

func TestSearchFallsBackWhenDisabled(t *testing.T) {
	// With no client, semantic search is disabled and Search must not panic;
	// it delegates to keyword search (which returns nil on an unset vault root).
	Init(nil)
	if Enabled() {
		t.Fatal("expected disabled with nil client")
	}
	if _, err := Related(nil, "whatever.md", 5); err != nil {
		t.Errorf("Related should no-op when disabled, got %v", err)
	}
}
