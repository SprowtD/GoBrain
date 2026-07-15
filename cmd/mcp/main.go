// Command mcp is a stdio MCP server exposing a secondbrain vault to any
// MCP-compatible agent (Claude Code, Cursor, ...). It calls a running
// secondbrain backend over HTTP using SECONDBRAIN_URL + SECONDBRAIN_TOKEN, so
// it works across harnesses and whether or not the vault is cloned locally.
//
// The tool surface and JSON-RPC framing live in internal/mcp, shared with the
// backend's built-in Streamable HTTP server (internal/api) so a stdio launch and
// a remote "add by URL" connector expose exactly the same tools. This binary is
// just the stdio transport plus an HTTP-backed mcp.Backend.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"secondbrain-server/internal/mcp"
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
	serve(os.Stdin, os.Stdout, httpBackend{})
}

// serve runs the line-delimited JSON-RPC loop of the MCP stdio transport,
// dispatching each request through the shared core.
func serve(in io.Reader, out io.Writer, b mcp.Backend) {
	reader := bufio.NewReader(in)
	writer := bufio.NewWriter(out)
	for {
		line, err := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			handleLine(line, writer, b)
			writer.Flush()
		}
		if err != nil {
			return
		}
	}
}

func handleLine(line []byte, w *bufio.Writer, b mcp.Backend) {
	var req mcp.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	result, rerr := mcp.Handle(context.Background(), req, b)
	if req.IsNotification() {
		return // notifications get no response
	}
	resp := mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	out, _ := json.Marshal(resp)
	w.Write(out)
	w.WriteByte('\n')
}

// --- HTTP-backed mcp.Backend: wraps the running secondbrain backend ---

type httpBackend struct{}

func (httpBackend) Search(_ context.Context, q string) ([]mcp.SearchHit, error) {
	body, code, err := apiGet("/v1/search?q=" + url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, backendErr(code, body)
	}
	var hits []struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Snippet string `json:"snippet"`
	}
	json.Unmarshal(body, &hits)
	out := make([]mcp.SearchHit, len(hits))
	for i, h := range hits {
		out[i] = mcp.SearchHit{Path: h.Path, Title: h.Title, Snippet: h.Snippet}
	}
	return out, nil
}

func (httpBackend) Read(_ context.Context, path string) (string, error) {
	body, code, err := apiGet("/v1/notes/" + path)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", mcp.ErrNotFound
	}
	if code != http.StatusOK {
		return "", backendErr(code, body)
	}
	var r struct {
		Content string `json:"content"`
	}
	json.Unmarshal(body, &r)
	return r.Content, nil
}

func (httpBackend) Related(_ context.Context, path string) ([]mcp.RelatedHit, error) {
	body, code, err := apiGet("/v1/related?path=" + url.QueryEscape(path))
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, mcp.ErrNotFound
	}
	if code != http.StatusOK {
		return nil, backendErr(code, body)
	}
	var hits []struct {
		Path  string `json:"path"`
		Title string `json:"title"`
	}
	json.Unmarshal(body, &hits)
	out := make([]mcp.RelatedHit, len(hits))
	for i, h := range hits {
		out[i] = mcp.RelatedHit{Path: h.Path, Title: h.Title}
	}
	return out, nil
}

func (httpBackend) Write(_ context.Context, in mcp.WriteInput) (string, error) {
	resp, code, err := apiPost("/v1/notes", map[string]any{
		"title": in.Title, "body": in.Body, "kind": in.Kind, "project": in.Project, "tags": in.Tags,
	})
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return "", backendErr(code, resp)
	}
	var r struct {
		Path string `json:"path"`
	}
	json.Unmarshal(resp, &r)
	return r.Path, nil
}

func (httpBackend) Delete(_ context.Context, path string) error {
	body, code, err := apiDelete("/v1/notes/" + path)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return mcp.ErrNotFound
	}
	if code != http.StatusNoContent && code != http.StatusOK {
		return backendErr(code, body)
	}
	return nil
}

func backendErr(code int, body []byte) error {
	return fmt.Errorf("backend %d: %s", code, strings.TrimSpace(string(body)))
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

func apiDelete(path string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodDelete, backendURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
