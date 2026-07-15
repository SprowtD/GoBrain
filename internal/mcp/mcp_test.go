package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeBackend is a minimal in-memory Backend for exercising the dispatch/render
// logic without a real vault.
type fakeBackend struct {
	notes map[string]string // path -> content
}

func (f *fakeBackend) Search(_ context.Context, q string) ([]SearchHit, error) {
	if q == "empty" {
		return nil, nil
	}
	return []SearchHit{{Path: "notes/a.md", Title: "A", Snippet: "snippet"}}, nil
}
func (f *fakeBackend) Read(_ context.Context, path string) (string, error) {
	if c, ok := f.notes[path]; ok {
		return c, nil
	}
	return "", ErrNotFound
}
func (f *fakeBackend) Related(_ context.Context, path string) ([]RelatedHit, error) {
	return nil, ErrNotFound
}
func (f *fakeBackend) Write(_ context.Context, in WriteInput) (string, error) {
	path := "projects/" + Slugify(in.Project) + "/" + Slugify(in.Title) + ".md"
	if f.notes == nil {
		f.notes = map[string]string{}
	}
	f.notes[path] = in.Body
	return path, nil
}
func (f *fakeBackend) Delete(_ context.Context, path string) error {
	if _, ok := f.notes[path]; !ok {
		return ErrNotFound
	}
	delete(f.notes, path)
	return nil
}

func call(t *testing.T, b Backend, name string, args map[string]any) (string, bool) {
	t.Helper()
	argsJSON, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(argsJSON)})
	res, rerr := Handle(context.Background(), Request{Method: "tools/call", Params: params, ID: json.RawMessage("1")}, b)
	if rerr != nil {
		t.Fatalf("tools/call %s returned rpc error: %v", name, rerr)
	}
	m := res.(map[string]any)
	text := m["content"].([]map[string]any)[0]["text"].(string)
	isErr, _ := m["isError"].(bool)
	return text, isErr
}

func TestToolsListSurface(t *testing.T) {
	res, rerr := Handle(context.Background(), Request{Method: "tools/list", ID: json.RawMessage("1")}, &fakeBackend{})
	if rerr != nil {
		t.Fatal(rerr)
	}
	tools := res.(map[string]any)["tools"].([]map[string]any)
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl["name"].(string)] = true
	}
	for _, want := range []string{"search_vault", "read_note", "related_notes", "write_note", "delete_note", "project_index"} {
		if !got[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	b := &fakeBackend{}
	out, isErr := call(t, b, "write_note", map[string]any{"title": "Hello World", "body": "the body", "project": "demo"})
	if isErr {
		t.Fatalf("write_note errored: %s", out)
	}
	got, isErr := call(t, b, "read_note", map[string]any{"path": "projects/demo/hello-world.md"})
	if isErr || got != "the body" {
		t.Fatalf("read_note = %q (isErr %v); want %q", got, isErr, "the body")
	}
}

func TestReadMissingRendersNotFound(t *testing.T) {
	text, isErr := call(t, &fakeBackend{}, "read_note", map[string]any{"path": "nope.md"})
	if isErr {
		t.Fatal("not-found should not be an isError result")
	}
	if text != "Not found: nope.md" {
		t.Fatalf("read_note missing = %q; want friendly not-found", text)
	}
}

func TestMissingRequiredArgIsError(t *testing.T) {
	text, isErr := call(t, &fakeBackend{}, "write_note", map[string]any{"title": "no body"})
	if !isErr {
		t.Fatalf("expected isError for missing body, got %q", text)
	}
}

func TestSearchEmptyAndHits(t *testing.T) {
	b := &fakeBackend{}
	if text, _ := call(t, b, "search_vault", map[string]any{"query": "empty"}); text != "No matches." {
		t.Fatalf("empty search = %q; want 'No matches.'", text)
	}
	if text, _ := call(t, b, "search_vault", map[string]any{"query": "x"}); text == "No matches." {
		t.Fatalf("non-empty search rendered no matches: %q", text)
	}
}
