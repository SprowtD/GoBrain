package vault

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// writeTagPages builds a hub page per tag under tags/, each linking to every
// note carrying that tag. In Obsidian these hubs connect all notes that share a
// tag, turning the vault into a navigable topical graph — without modifying the
// model-written note bodies. Regenerated from scratch each commit so removed
// tags leave no stale pages; identical output produces no git diff.
func writeTagPages() error {
	tagsDir := filepath.Join(rootPath, "tags")

	type note struct {
		path  string
		title string
	}
	tagMap := map[string][]note{}

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
		title, tags := readFrontmatter(path)
		if title == "" {
			title = strings.TrimSuffix(d.Name(), ".md")
		}
		for _, t := range tags {
			tagMap[t] = append(tagMap[t], note{path: path, title: title})
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Rebuild tags/ from scratch so dropped tags don't leave dead pages.
	if err := removeMarkdown(tagsDir); err != nil {
		return err
	}
	if len(tagMap) == 0 {
		_ = os.Remove(tagsDir) // best-effort: remove if now empty
		return nil
	}
	if err := os.MkdirAll(tagsDir, 0o755); err != nil {
		return err
	}

	for tag, notes := range tagMap {
		sort.Slice(notes, func(i, j int) bool { return notes[i].title < notes[j].title })

		var b strings.Builder
		b.WriteString("---\n")
		b.WriteString("type: \"Tag\"\n")
		fmt.Fprintf(&b, "title: %q\n", tag)
		b.WriteString("---\n\n")
		fmt.Fprintf(&b, "# %s\n\n", tag)
		for _, n := range notes {
			rel, err := filepath.Rel(tagsDir, n.path)
			if err != nil {
				rel = n.path
			}
			fmt.Fprintf(&b, "- [%s](%s)\n", n.title, filepath.ToSlash(rel))
		}

		if err := os.WriteFile(filepath.Join(tagsDir, tagSlug(tag)+".md"), []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// readFrontmatter extracts title and tags from our YAML frontmatter block. It
// only understands the shape we write (title: "...", tags: ["a", "b"]).
func readFrontmatter(path string) (title string, tags []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", nil // no frontmatter
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		switch {
		case strings.HasPrefix(line, "title:"):
			title = unquoteYAML(strings.TrimPrefix(line, "title:"))
		case strings.HasPrefix(line, "tags:"):
			tags = parseTagList(strings.TrimPrefix(line, "tags:"))
		}
	}
	return title, tags
}

func parseTagList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		p = strings.ReplaceAll(p, `\"`, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\"`, `"`)
	}
	return s
}

func removeMarkdown(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func tagSlug(s string) string {
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
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "tag"
	}
	return out
}
