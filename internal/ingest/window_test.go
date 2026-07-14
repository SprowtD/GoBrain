package ingest

import (
	"strings"
	"testing"
)

func TestWindowTextShort(t *testing.T) {
	text := "one\ntwo\nthree"
	got := windowText(text, 1000)
	if len(got) != 1 {
		t.Fatalf("short text should be 1 window, got %d", len(got))
	}
	if got[0] != text {
		t.Fatalf("window content changed: %q", got[0])
	}
}

func TestWindowTextSplitsAndPreservesContent(t *testing.T) {
	// 100 lines of ~20 runes each ≈ 2000 runes; window at 500 forces splits.
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "line-with-some-words")
	}
	text := strings.Join(lines, "\n")

	windows := windowText(text, 500)
	if len(windows) < 2 {
		t.Fatalf("expected multiple windows, got %d", len(windows))
	}
	for i, w := range windows {
		if len([]rune(w)) > 500 {
			t.Errorf("window %d exceeds cap: %d runes", i, len([]rune(w)))
		}
	}
	// Every original line must survive somewhere, in order, with no loss.
	recombined := strings.Join(windows, "\n")
	gotLines := strings.Fields(recombined)
	if len(gotLines) != 100 {
		t.Fatalf("expected 100 lines preserved, got %d", len(gotLines))
	}
}

func TestWindowTextHardSplitsLongLine(t *testing.T) {
	long := strings.Repeat("x", 1200) // single line, no newlines
	windows := windowText(long, 500)
	if len(windows) != 3 {
		t.Fatalf("expected 3 hard-split windows (500+500+200), got %d", len(windows))
	}
	total := 0
	for _, w := range windows {
		n := len([]rune(w))
		if n > 500 {
			t.Errorf("hard-split window too big: %d", n)
		}
		total += n
	}
	if total != 1200 {
		t.Errorf("hard-split lost content: %d runes total, want 1200", total)
	}
}
