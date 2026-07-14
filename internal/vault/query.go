package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when a requested note does not exist.
var ErrNotFound = errors.New("note not found")

// safeJoin resolves a vault-relative path and refuses anything that escapes the
// vault root (path traversal protection).
func safeJoin(rel string) (string, error) {
	clean := filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(rel), "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("empty path")
	}
	full := filepath.Join(rootPath, clean)
	rp, err := filepath.Rel(rootPath, full)
	if err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes vault")
	}
	return full, nil
}

// ReadNote returns the contents of a note by vault-relative path.
func ReadNote(rel string) (string, error) {
	full, err := safeJoin(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return string(data), nil
}

// SearchHit is one matching note.
type SearchHit struct {
	Path    string  `json:"path"`
	Title   string  `json:"title"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score,omitempty"` // cosine similarity for semantic hits
}

// Note is a real vault note enumerated for indexing (derived pages excluded).
type Note struct {
	Path  string // vault-relative, slash-separated
	Title string
	Text  string // full file contents
}

// ListNotes returns every author-written note in the vault, skipping the
// derived navigation pages (index.md and the tags/ hubs) so the semantic index
// only embeds real content.
func ListNotes() ([]Note, error) {
	tagsDir := filepath.Join(rootPath, "tags")
	var notes []Note
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || path == tagsDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") || d.Name() == "index.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable file, keep going
		}
		title, _ := readFrontmatter(path)
		rel, _ := filepath.Rel(rootPath, path)
		if title == "" {
			title = strings.TrimSuffix(d.Name(), ".md")
		}
		notes = append(notes, Note{
			Path:  filepath.ToSlash(rel),
			Title: title,
			Text:  string(data),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// NoteTitle returns a note's frontmatter title (falling back to its filename)
// for a vault-relative path. Used to label semantic-search hits.
func NoteTitle(rel string) (string, error) {
	full, err := safeJoin(rel)
	if err != nil {
		return "", err
	}
	title, _ := readFrontmatter(full)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(rel), ".md")
	}
	return title, nil
}

// Excerpt returns a compact leading snippet of a note's body (frontmatter
// stripped), used for semantic-search hits where there's no keyword to anchor on.
func Excerpt(text string, max int) string {
	body := text
	// Skip a leading YAML frontmatter block if present.
	if strings.HasPrefix(body, "---\n") {
		if end := strings.Index(body[4:], "\n---"); end >= 0 {
			body = body[4+end+4:]
		}
	}
	body = strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))
	if max > 0 {
		r := []rune(body)
		if len(r) > max {
			return strings.TrimSpace(string(r[:max])) + "…"
		}
	}
	return body
}

// Search does a case-insensitive substring scan over every note's text and
// returns up to `limit` hits. (Embeddings would be a future upgrade.)
func Search(query string, limit int) ([]SearchHit, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}

	var hits []SearchHit
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)
		if !strings.Contains(strings.ToLower(text), q) {
			return nil
		}
		title, _ := readFrontmatter(path)
		if title == "" {
			title = strings.TrimSuffix(d.Name(), ".md")
		}
		rel, _ := filepath.Rel(rootPath, path)
		hits = append(hits, SearchHit{
			Path:    filepath.ToSlash(rel),
			Title:   title,
			Snippet: snippet(text, q),
		})
		if len(hits) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

func snippet(text, q string) string {
	i := strings.Index(strings.ToLower(text), q)
	if i < 0 {
		return ""
	}
	start := i - 60
	if start < 0 {
		start = 0
	}
	end := i + len(q) + 100
	if end > len(text) {
		end = len(text)
	}
	return strings.ReplaceAll(strings.TrimSpace(text[start:end]), "\n", " ")
}
