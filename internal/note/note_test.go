package note

import (
	"strings"
	"testing"
)

func TestRenderProjectNote(t *testing.T) {
	n := Note{
		Kind: "Design Decision", Title: "Token Roles: admin vs member",
		Tags: []string{"auth", "design"}, Body: "We chose a role column.",
		Project: "2brain", Author: "ashley-laptop",
	}
	rel, content := n.Render("2026-07-13T00:00:00Z")

	if !strings.HasPrefix(rel, "projects/2brain/token-roles-admin-vs-member-") || !strings.HasSuffix(rel, ".md") {
		t.Fatalf("unexpected path: %s", rel)
	}
	for _, want := range []string{
		`type: "Design Decision"`,
		`title: "Token Roles: admin vs member"`,
		`tags: ["auth", "design"]`,
		`project: "2brain"`,
		`author: "ashley-laptop"`,
		"We chose a role column.",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q:\n%s", want, content)
		}
	}
}

func TestRenderNoProjectDefaultsToNotesDir(t *testing.T) {
	rel, _ := Note{Title: "Loose thought", Body: "x"}.Render("")
	if !strings.HasPrefix(rel, "notes/loose-thought-") {
		t.Fatalf("expected notes/ dir, got %s", rel)
	}
}

func TestRenderDefaultsKind(t *testing.T) {
	_, content := Note{Title: "t", Body: "b"}.Render("")
	if !strings.Contains(content, `type: "Note"`) {
		t.Errorf("expected default type Note:\n%s", content)
	}
}
