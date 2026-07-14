package vault

import (
	"strings"
	"testing"
)

func TestStripManagedBlockIsInverseOfAppend(t *testing.T) {
	base := "---\ntitle: \"T\"\n---\n\nThe real body of the note."
	block := buildRelatedBlock([]RelatedItem{
		{Path: "projects/x/a-ab12.md", Title: "Note A"},
		{Path: "b-cd34.md", Title: "Note B"},
	})
	withBlock := base + "\n\n" + block + "\n"

	// Stripping the block must recover the original body exactly (trailing
	// newline aside) — this is what keeps the embedding hash stable.
	got := strings.TrimRight(StripManagedBlock(withBlock), "\n")
	if got != base {
		t.Errorf("strip did not recover base:\n got=%q\nwant=%q", got, base)
	}
}

func TestStripManagedBlockNoBlockIsNoop(t *testing.T) {
	text := "---\ntitle: \"T\"\n---\n\njust a body, no related block"
	if StripManagedBlock(text) != text {
		t.Error("StripManagedBlock changed text with no managed block")
	}
}

func TestBuildRelatedBlock(t *testing.T) {
	if buildRelatedBlock(nil) != "" {
		t.Error("empty items should yield empty block")
	}
	block := buildRelatedBlock([]RelatedItem{{Path: "dir/my-note-ab12.md", Title: "My Note"}})
	for _, want := range []string{relatedStart, relatedEnd, "## Related", "[[my-note-ab12|My Note]]"} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
}

func TestStripHandlesMalformedBlock(t *testing.T) {
	// Start marker but no end marker — strip everything from the marker on.
	text := "body here\n\n" + relatedStart + "\n## Related\n- [[x|X]]\n(truncated)"
	got := strings.TrimRight(StripManagedBlock(text), "\n")
	if got != "body here" {
		t.Errorf("malformed strip = %q, want \"body here\"", got)
	}
}
