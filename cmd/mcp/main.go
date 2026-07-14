// Command mcp is a stdio MCP server exposing a secondbrain vault to any
// MCP-compatible agent (Claude Code, Cursor, ...). It calls a running
// secondbrain backend over HTTP using SECONDBRAIN_URL + SECONDBRAIN_TOKEN, so
// it works across harnesses and whether or not the vault is cloned locally.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

var (
	backendURL string
	token      string
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

func main() {
	backendURL = strings.TrimRight(os.Getenv("SECONDBRAIN_URL"), "/")
	token = os.Getenv("SECONDBRAIN_TOKEN")
	if backendURL == "" || token == "" {
		fmt.Fprintln(os.Stderr, "mcp: set SECONDBRAIN_URL and SECONDBRAIN_TOKEN")
		os.Exit(1)
	}
	serve(os.Stdin, os.Stdout)
}

// --- JSON-RPC 2.0 (line-delimited, per the MCP stdio transport) ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serve(in io.Reader, out io.Writer) {
	reader := bufio.NewReader(in)
	writer := bufio.NewWriter(out)
	for {
		line, err := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			handleLine(line, writer)
			writer.Flush()
		}
		if err != nil {
			return
		}
	}
}

func handleLine(line []byte, w *bufio.Writer) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	isNotification := len(req.ID) == 0
	result, rerr := dispatch(req)
	if isNotification {
		return // notifications get no response
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	b, _ := json.Marshal(resp)
	w.Write(b)
	w.WriteByte('\n')
}

func dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return initialize(req.Params), nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		return toolsCall(req.Params)
	case "ping":
		return map[string]any{}, nil
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return nil, nil
		}
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func initialize(params json.RawMessage) any {
	version := "2024-11-05"
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

func toolDefs() []map[string]any {
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

func toolsCall(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	text, err := runTool(p.Name, p.Arguments)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "Error: " + err.Error()}},
			"isError": true,
		}, nil
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
}

func runTool(name string, args json.RawMessage) (string, error) {
	switch name {
	case "search_vault":
		var a struct {
			Query string `json:"query"`
		}
		json.Unmarshal(args, &a)
		if a.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		return doSearch(a.Query)

	case "read_note":
		var a struct {
			Path string `json:"path"`
		}
		json.Unmarshal(args, &a)
		if a.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		return doRead(a.Path)

	case "related_notes":
		var a struct {
			Path string `json:"path"`
		}
		json.Unmarshal(args, &a)
		if a.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		return doRelated(a.Path)

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
		return doWrite(a.Title, a.Body, a.Kind, a.Project, a.Tags)

	case "project_index":
		var a struct {
			Project string `json:"project"`
		}
		json.Unmarshal(args, &a)
		if a.Project == "" {
			return "", fmt.Errorf("project is required")
		}
		return doRead("projects/" + slugify(a.Project) + "/index.md")

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func doSearch(q string) (string, error) {
	body, code, err := apiGet("/v1/search?q=" + url.QueryEscape(q))
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("backend %d: %s", code, strings.TrimSpace(string(body)))
	}
	var hits []struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Snippet string `json:"snippet"`
	}
	json.Unmarshal(body, &hits)
	if len(hits) == 0 {
		return "No matches.", nil
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s  (%s)\n  %s\n", h.Title, h.Path, h.Snippet)
	}
	return b.String(), nil
}

func doRelated(path string) (string, error) {
	body, code, err := apiGet("/v1/related?path=" + url.QueryEscape(path))
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "Not found: " + path, nil
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("backend %d: %s", code, strings.TrimSpace(string(body)))
	}
	var hits []struct {
		Path  string `json:"path"`
		Title string `json:"title"`
	}
	json.Unmarshal(body, &hits)
	if len(hits) == 0 {
		return "No related notes (semantic search may be disabled).", nil
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s  (%s)\n", h.Title, h.Path)
	}
	return b.String(), nil
}

func doRead(path string) (string, error) {
	body, code, err := apiGet("/v1/notes/" + path)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "Not found: " + path, nil
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("backend %d: %s", code, strings.TrimSpace(string(body)))
	}
	var r struct {
		Content string `json:"content"`
	}
	json.Unmarshal(body, &r)
	return r.Content, nil
}

func doWrite(title, body, kind, project string, tags []string) (string, error) {
	resp, code, err := apiPost("/v1/notes", map[string]any{
		"title": title, "body": body, "kind": kind, "project": project, "tags": tags,
	})
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return "", fmt.Errorf("backend %d: %s", code, strings.TrimSpace(string(resp)))
	}
	var r struct {
		Path string `json:"path"`
	}
	json.Unmarshal(resp, &r)
	return "Wrote note: " + r.Path, nil
}

// --- HTTP helpers ---

func apiGet(path string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, backendURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return do(req)
}

func apiPost(path string, payload any) ([]byte, int, error) {
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, backendURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return do(req)
}

func do(req *http.Request) ([]byte, int, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.StatusCode, nil
}

// slugify mirrors the backend's project slug so project_index resolves the right
// directory.
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
	}
	return strings.Trim(b.String(), "-")
}
