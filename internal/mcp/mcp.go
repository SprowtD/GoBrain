// Package mcp is the transport-agnostic core of the second-brain MCP server:
// the tool schemas and the JSON-RPC dispatch, shared by both transports so they
// always expose an identical tool surface.
//
//   - cmd/mcp (stdio) drives it with an HTTP-backed Backend, for local launches.
//   - internal/api (Streamable HTTP) drives it with an in-process Backend, so any
//     agent can add the vault by URL with no local build.
//
// A Backend only fetches/mutates data; all JSON-RPC framing, argument
// validation, and text rendering live here, so the two transports can never
// drift.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrNotFound signals a missing note/path so tools render a friendly message
// instead of a hard error. Backends must map their own not-found to this.
var ErrNotFound = errors.New("not found")

// ProtocolVersion is the MCP revision we default to when a client doesn't pin one.
const ProtocolVersion = "2024-11-05"

// SearchHit and RelatedHit are the shapes the tools render to text.
type SearchHit struct {
	Path    string
	Title   string
	Snippet string
}

type RelatedHit struct {
	Path  string
	Title string
}

// WriteInput carries the fields of the write_note tool.
type WriteInput struct {
	Title   string
	Body    string
	Kind    string
	Project string
	Tags    []string
}

// Backend performs the actual vault actions behind the tools. Implementations
// must return ErrNotFound for missing paths.
type Backend interface {
	Search(ctx context.Context, query string) ([]SearchHit, error)
	Read(ctx context.Context, path string) (string, error)
	Related(ctx context.Context, path string) ([]RelatedHit, error)
	Write(ctx context.Context, in WriteInput) (path string, err error)
	Delete(ctx context.Context, path string) error
}

// --- JSON-RPC 2.0 (per the MCP spec) ---

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// IsNotification reports whether a request expects no response (no id).
func (r Request) IsNotification() bool { return len(r.ID) == 0 }

// Handle dispatches a single JSON-RPC request and returns either a result or an
// error object. Notifications still route through here (e.g. notifications/*),
// returning (nil, nil); callers use IsNotification to decide whether to reply.
func Handle(ctx context.Context, req Request, b Backend) (any, *Error) {
	switch req.Method {
	case "initialize":
		return initialize(req.Params), nil
	case "tools/list":
		return map[string]any{"tools": ToolDefs()}, nil
	case "tools/call":
		return toolsCall(ctx, req.Params, b)
	case "ping":
		return map[string]any{}, nil
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return nil, nil
		}
		return nil, &Error{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func initialize(params json.RawMessage) any {
	version := ProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "secondbrain", "version": "0.1.0"},
	}
}

// --- tools ---

// ToolDefs returns the JSON-Schema tool definitions advertised to clients. This
// is the single source of truth for both transports.
func ToolDefs() []map[string]any {
	strProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []map[string]any{
		{
			"name":        "search_vault",
			"description": "Semantic search over the shared second-brain vault (falls back to keyword when embeddings aren't configured). Returns matching notes with path, title, and a snippet.",
			"inputSchema": obj(map[string]any{"query": strProp("What you're looking for — a phrase or question, not just keywords")}, "query"),
		},
		{
			"name":        "read_note",
			"description": "Read a note from the vault by its path (as returned by search_vault or write_note).",
			"inputSchema": obj(map[string]any{"path": strProp("Vault-relative path, e.g. projects/2brain/foo-ab12cd.md")}, "path"),
		},
		{
			"name":        "related_notes",
			"description": "Find notes semantically related to a given note (nearest neighbours by meaning). Use after read_note to pull in adjacent context. Returns [] if semantic search isn't configured.",
			"inputSchema": obj(map[string]any{"path": strProp("Vault-relative path of the note to find neighbours for")}, "path"),
		},
		{
			"name":        "delete_note",
			"description": "Delete a note from the vault by its path. The vault is git-backed, so deletions are recoverable from history. Use sparingly.",
			"inputSchema": obj(map[string]any{"path": strProp("Vault-relative path of the note to delete")}, "path"),
		},
		{
			"name":        "write_note",
			"description": "Write a structured note into the vault (design idea, decision, code snippet, session memory), grouped under a project. Use this to share knowledge with the team's other agents.",
			"inputSchema": obj(map[string]any{
				"title":   strProp("Short note title"),
				"body":    strProp("Markdown body"),
				"kind":    strProp("OKF type, e.g. Design Decision, Memory, Snippet (default Note)"),
				"project": strProp("Project to file under, e.g. 2brain"),
				"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Lowercase keyword tags"},
			}, "title", "body"),
		},
		{
			"name":        "project_index",
			"description": "Read a project's index (the table of contents of its notes).",
			"inputSchema": obj(map[string]any{"project": strProp("Project name, e.g. 2brain")}, "project"),
		},
	}
}

func toolsCall(ctx context.Context, params json.RawMessage, b Backend) (any, *Error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &Error{Code: -32602, Message: "invalid params"}
	}
	text, err := runTool(ctx, p.Name, p.Arguments, b)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "Error: " + err.Error()}},
			"isError": true,
		}, nil
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
}

// runTool validates arguments, invokes the Backend, and renders the result to
// the plain text an agent reads. Shared by both transports.
func runTool(ctx context.Context, name string, args json.RawMessage, b Backend) (string, error) {
	switch name {
	case "search_vault":
		var a struct {
			Query string `json:"query"`
		}
		json.Unmarshal(args, &a)
		if a.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		hits, err := b.Search(ctx, a.Query)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "No matches.", nil
		}
		var sb strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&sb, "- %s  (%s)\n  %s\n", h.Title, h.Path, h.Snippet)
		}
		return sb.String(), nil

	case "read_note":
		var a struct {
			Path string `json:"path"`
		}
		json.Unmarshal(args, &a)
		if a.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		content, err := b.Read(ctx, a.Path)
		if errors.Is(err, ErrNotFound) {
			return "Not found: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		return content, nil

	case "related_notes":
		var a struct {
			Path string `json:"path"`
		}
		json.Unmarshal(args, &a)
		if a.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		hits, err := b.Related(ctx, a.Path)
		if errors.Is(err, ErrNotFound) {
			return "Not found: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "No related notes (semantic search may be disabled).", nil
		}
		var sb strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&sb, "- %s  (%s)\n", h.Title, h.Path)
		}
		return sb.String(), nil

	case "delete_note":
		var a struct {
			Path string `json:"path"`
		}
		json.Unmarshal(args, &a)
		if a.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		err := b.Delete(ctx, a.Path)
		if errors.Is(err, ErrNotFound) {
			return "Not found: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		return "Deleted note: " + a.Path, nil

	case "write_note":
		var a struct {
			Title   string   `json:"title"`
			Body    string   `json:"body"`
			Kind    string   `json:"kind"`
			Project string   `json:"project"`
			Tags    []string `json:"tags"`
		}
		json.Unmarshal(args, &a)
		if a.Title == "" || a.Body == "" {
			return "", fmt.Errorf("title and body are required")
		}
		path, err := b.Write(ctx, WriteInput{
			Title: a.Title, Body: a.Body, Kind: a.Kind, Project: a.Project, Tags: a.Tags,
		})
		if err != nil {
			return "", err
		}
		return "Wrote note: " + path, nil

	case "project_index":
		var a struct {
			Project string `json:"project"`
		}
		json.Unmarshal(args, &a)
		if a.Project == "" {
			return "", fmt.Errorf("project is required")
		}
		content, err := b.Read(ctx, "projects/"+Slugify(a.Project)+"/index.md")
		if errors.Is(err, ErrNotFound) {
			return "No index for project: " + a.Project, nil
		}
		if err != nil {
			return "", err
		}
		return content, nil

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// Slugify mirrors the backend's project slug so project_index resolves the right
// directory. Kept in sync with note.slugify.
func Slugify(s string) string {
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
	return strings.Trim(b.String(), "-")
}
