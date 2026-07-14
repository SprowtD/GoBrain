package vault

import "testing"

func TestListNotesSkipsDerivedPages(t *testing.T) {
	dir := t.TempDir()
	rootPath = dir
	seedNote(t, dir, "projects/x/real.md",
		"---\ntitle: \"Real Note\"\ntags: [\"go\"]\n---\n\nbody text")
	seedNote(t, dir, "projects/x/index.md", "---\ntitle: \"x\"\n---\n\n# x")
	seedNote(t, dir, "tags/go.md", "---\ntitle: \"go\"\n---\n\n# go")

	notes, err := ListNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("expected 1 real note, got %d: %+v", len(notes), notes)
	}
	n := notes[0]
	if n.Path != "projects/x/real.md" || n.Title != "Real Note" {
		t.Errorf("unexpected note: %+v", n)
	}
	if n.Text == "" {
		t.Error("expected note text to be populated")
	}
}

func TestExcerptStripsFrontmatter(t *testing.T) {
	text := "---\ntitle: \"T\"\ntags: [\"a\"]\n---\n\nHello there, this is the body."
	got := Excerpt(text, 100)
	if got != "Hello there, this is the body." {
		t.Errorf("Excerpt = %q", got)
	}

	long := "---\ntitle: \"T\"\n---\n\n" + string(make([]byte, 0)) + "abcdefghij"
	if e := Excerpt(long, 4); e != "abcd…" {
		t.Errorf("truncated Excerpt = %q, want abcd…", e)
	}
}

func TestNoteTitleFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	rootPath = dir
	seedNote(t, dir, "notes/no-frontmatter.md", "just a body, no frontmatter")

	title, err := NoteTitle("notes/no-frontmatter.md")
	if err != nil {
		t.Fatal(err)
	}
	if title != "no-frontmatter" {
		t.Errorf("title = %q, want filename fallback", title)
	}
}
