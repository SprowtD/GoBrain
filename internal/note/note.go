// Package note renders agent-authored notes (design ideas, decisions, code
// snippets, session memories) as OKF markdown for direct, un-chunked vault
// writes — distinct from the ingestion pipeline which processes raw sources.
package note

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

type Note struct {
	Kind    string   // OKF type, e.g. "Design Decision", "Memory", "Snippet"
	Title   string
	Tags    []string
	Body    string
	Project string // groups notes under projects/<slug>/
	Author  string // token label, for attribution
}

// Render returns the vault-relative path and OKF markdown for the note.
func (n Note) Render(timestamp string) (relPath, content string) {
	kind := strings.TrimSpace(n.Kind)
	if kind == "" {
		kind = "Note"
	}
	title := strings.TrimSpace(n.Title)
	if title == "" {
		title = "Untitled"
	}

	fname := slugify(title) + "-" + shortID() + ".md"
	if p := slugify(n.Project); n.Project != "" && p != "untitled" {
		relPath = filepath.Join("projects", p, fname)
	} else {
		relPath = filepath.Join("notes", fname)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "type: %s\n", yamlStr(kind))
	fmt.Fprintf(&b, "title: %s\n", yamlStr(title))
	fmt.Fprintf(&b, "tags: %s\n", yamlTags(n.Tags))
	if timestamp != "" {
		fmt.Fprintf(&b, "timestamp: %s\n", timestamp)
	}
	fmt.Fprintf(&b, "source_kind: %s\n", yamlStr("note"))
	if n.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", yamlStr(n.Project))
	}
	if n.Author != "" {
		fmt.Fprintf(&b, "author: %s\n", yamlStr(n.Author))
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(n.Body))
	b.WriteString("\n")
	return relPath, b.String()
}

func shortID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}

func slugify(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastHyphen = false
		default:
			if b.Len() > 0 && !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		if len([]rune(b.String())) >= 60 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	return out
}

func yamlStr(s string) string {
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
		if t = strings.TrimSpace(t); t != "" {
			quoted = append(quoted, yamlStr(t))
		}
	}
	if len(quoted) == 0 {
		return "[]"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
