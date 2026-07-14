package ingest

import (
	"fmt"
	"strings"
)

// fileMeta carries the per-file metadata that becomes OKF frontmatter.
// See Google's Open Knowledge Format v0.1: markdown + YAML frontmatter where
// `type` is the only mandatory field and producers may add their own fields.
type fileMeta struct {
	Type       string // OKF: mandatory concept type, e.g. "Article", "Note"
	Resource   string // OKF: URL to the authoritative source
	Timestamp  string // OKF: ISO 8601 last-modified
	SourceKind string // producer field: article|youtube|image|thought
	JobID      string // producer field
	Site       string // producer field
	Byline     string // producer field
	Note       string // producer field
	Model      string // producer field
}

// renderChunk produces an OKF-compliant Markdown file: YAML frontmatter with
// the canonical OKF fields first, our producer-specific fields after, then the
// chunk body.
func renderChunk(m fileMeta, c Chunk) string {
	var b strings.Builder
	b.WriteString("---\n")

	// --- OKF canonical fields ---
	fmt.Fprintf(&b, "type: %s\n", yamlString(m.Type))
	fmt.Fprintf(&b, "title: %s\n", yamlString(c.Title))
	if c.Summary != "" {
		fmt.Fprintf(&b, "description: %s\n", yamlString(c.Summary))
	}
	if m.Resource != "" {
		fmt.Fprintf(&b, "resource: %s\n", yamlString(m.Resource))
	}
	fmt.Fprintf(&b, "tags: %s\n", yamlTags(c.Tags))
	if m.Timestamp != "" {
		fmt.Fprintf(&b, "timestamp: %s\n", m.Timestamp) // RFC3339 is a valid bare YAML timestamp
	}

	// --- producer-specific fields (OKF allows arbitrary extra fields) ---
	fmt.Fprintf(&b, "source_kind: %s\n", yamlString(m.SourceKind))
	fmt.Fprintf(&b, "job_id: %s\n", yamlString(m.JobID))
	if m.Site != "" {
		fmt.Fprintf(&b, "site: %s\n", yamlString(m.Site))
	}
	if m.Byline != "" {
		fmt.Fprintf(&b, "byline: %s\n", yamlString(m.Byline))
	}
	if m.Note != "" {
		fmt.Fprintf(&b, "note: %s\n", yamlString(m.Note))
	}
	if m.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", yamlString(m.Model))
	}

	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(c.Body))
	b.WriteString("\n")
	return b.String()
}

// yamlString double-quotes a scalar and neutralizes quotes/newlines so a value
// with a colon, '#', or line break can't break the frontmatter block.
func yamlString(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func yamlTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		quoted = append(quoted, yamlString(t))
	}
	if len(quoted) == 0 {
		return "[]"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		return "Untitled"
	}
	return s
}

// okfType maps our source kinds to human-readable OKF concept types.
func okfType(sourceKind string) string {
	switch sourceKind {
	case "article":
		return "Article"
	case "youtube":
		return "Video Transcript"
	case "image":
		return "Image"
	case "thought":
		return "Note"
	default:
		return sourceKind
	}
}
