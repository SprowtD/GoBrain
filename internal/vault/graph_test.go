package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTagPages(t *testing.T) {
	dir := t.TempDir()
	rootPath = dir // package-level target for writeTagPages

	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("article/worker-pools-abc123/01-intro.md",
		"---\ntype: \"Article\"\ntitle: \"Worker Pools\"\ntags: [\"go\", \"concurrency\"]\n---\n\nbody\n")
	write("thought/go-tip-def456.md",
		"---\ntype: \"Note\"\ntitle: \"Go tip\"\ntags: [\"go\"]\n---\n\nbody\n")
	// A note with no tags must not appear anywhere.
	write("thought/untagged-ghi789.md",
		"---\ntype: \"Note\"\ntitle: \"Untagged\"\ntags: []\n---\n\nbody\n")

	if err := writeTagPages(); err != nil {
		t.Fatalf("writeTagPages: %v", err)
	}

	goPage, err := os.ReadFile(filepath.Join(dir, "tags", "go.md"))
	if err != nil {
		t.Fatalf("expected tags/go.md: %v", err)
	}
	s := string(goPage)
	for _, want := range []string{
		"# go",
		"[Go tip](../thought/go-tip-def456.md)",
		"[Worker Pools](../article/worker-pools-abc123/01-intro.md)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("tags/go.md missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Untagged") {
		t.Errorf("untagged note should not appear in a tag page:\n%s", s)
	}

	if _, err := os.ReadFile(filepath.Join(dir, "tags", "concurrency.md")); err != nil {
		t.Errorf("expected tags/concurrency.md: %v", err)
	}

	// Removing all tags should tear the tags/ dir back down (no stale pages).
	write("article/worker-pools-abc123/01-intro.md",
		"---\ntype: \"Article\"\ntitle: \"Worker Pools\"\ntags: []\n---\n\nbody\n")
	write("thought/go-tip-def456.md",
		"---\ntype: \"Note\"\ntitle: \"Go tip\"\ntags: []\n---\n\nbody\n")
	if err := writeTagPages(); err != nil {
		t.Fatalf("writeTagPages (empty): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tags")); !os.IsNotExist(err) {
		t.Errorf("tags/ dir should be gone when no tags remain, got err=%v", err)
	}
}
