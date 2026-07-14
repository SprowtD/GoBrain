package ingest

import (
	"strings"
	"unicode"
)

const maxSlugRunes = 60

// slugify turns a title into a filesystem/URL-friendly slug: lowercase, letters
// and digits kept (unicode-aware), everything else collapsed to single hyphens.
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
		if len([]rune(b.String())) >= maxSlugRunes {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	return out
}

// shortID is a stable short suffix from the job ID, used to guarantee unique
// file/folder names without a filesystem race.
func shortID(id string) string {
	if len(id) >= 6 {
		return id[:6]
	}
	return id
}
