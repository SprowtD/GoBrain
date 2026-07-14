package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A note's "Related" section is machine-managed and regenerated from embeddings,
// so it's wrapped in HTML-comment markers. Everything between them is owned by
// the indexer; the model-written body above is never touched. The block is
// stripped before hashing/embedding a note, so regenerating it never triggers a
// re-embed (which would loop).
const (
	relatedStart = "<!-- gobrain:related:start -->"
	relatedEnd   = "<!-- gobrain:related:end -->"
)

// RelatedItem is one neighbour link for a note's Related block.
type RelatedItem struct {
	Path  string // vault-relative path of the related note
	Title string
}

// StripManagedBlock returns the note text with the managed Related block (and
// its surrounding blank lines) removed. Safe on notes that have no block.
func StripManagedBlock(text string) string {
	i := strings.Index(text, relatedStart)
	if i < 0 {
		return text
	}
	end := strings.Index(text, relatedEnd)
	if end < 0 {
		end = len(text) // malformed/truncated block — drop to the end
	} else {
		end += len(relatedEnd)
	}
	before := strings.TrimRight(text[:i], " \t\n")
	after := strings.TrimLeft(text[end:], " \t\n")
	if after == "" {
		return before + "\n"
	}
	return before + "\n\n" + after
}

// SetRelated rewrites a note's managed Related block from items (empty items
// removes the block). It only writes when the file content actually changes, so
// a stable vault produces no churn and no commit loop. Returns whether it wrote.
func SetRelated(rel string, items []RelatedItem) (bool, error) {
	full, err := safeJoin(rel)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return false, ErrNotFound
		}
		return false, err
	}

	block := buildRelatedBlock(items)

	// Nothing to add and no existing block to remove — leave the file untouched
	// so we don't churn every note's trailing whitespace.
	if block == "" && !strings.Contains(string(data), relatedStart) {
		return false, nil
	}

	base := strings.TrimRight(StripManagedBlock(string(data)), "\n")

	var next string
	if block == "" {
		next = base + "\n"
	} else {
		next = base + "\n\n" + block + "\n"
	}
	if next == string(data) {
		return false, nil // unchanged — no write
	}
	return true, WriteSync(WriteRequest{RelPath: rel, Content: next})
}

func buildRelatedBlock(items []RelatedItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(relatedStart)
	b.WriteString("\n## Related\n")
	for _, it := range items {
		name := strings.TrimSuffix(filepath.Base(it.Path), ".md")
		title := it.Title
		if title == "" {
			title = name
		}
		// Obsidian-style wikilink resolved by note name, with the title as alias.
		fmt.Fprintf(&b, "- [[%s|%s]]\n", name, title)
	}
	b.WriteString(relatedEnd)
	return b.String()
}
