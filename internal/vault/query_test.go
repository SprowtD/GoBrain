package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func seedNote(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadNoteAndTraversal(t *testing.T) {
	dir := t.TempDir()
	rootPath = dir
	seedNote(t, dir, "projects/x/a.md", "hello world")

	got, err := ReadNote("projects/x/a.md")
	if err != nil || got != "hello world" {
		t.Fatalf("ReadNote = %q, %v", got, err)
	}

	if _, err := ReadNote("../../etc/passwd"); err == nil {
		t.Error("expected traversal to be rejected")
	}
	if _, err := ReadNote("projects/x/missing.md"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSearch(t *testing.T) {
	dir := t.TempDir()
	rootPath = dir
	seedNote(t, dir, "projects/x/pool.md",
		"---\ntitle: \"Worker Pools\"\ntags: [\"go\"]\n---\n\nA worker pool bounds concurrency.")
	seedNote(t, dir, "notes/other.md",
		"---\ntitle: \"Unrelated\"\n---\n\nsomething about databases")

	hits, err := Search("concurrency", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Title != "Worker Pools" || hits[0].Path != "projects/x/pool.md" {
		t.Errorf("unexpected hit: %+v", hits[0])
	}
	if hits[0].Snippet == "" {
		t.Error("expected a snippet")
	}

	if h, _ := Search("nonexistentterm", 10); len(h) != 0 {
		t.Errorf("expected no hits, got %d", len(h))
	}
}
