package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"secondbrain-server/internal/llm"
)

// Chunk is one topically-coherent unit extracted from a source document.
type Chunk struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Tags    []string `json:"tags"`
	Body    string   `json:"body"`
}

// Per-window input cap (runes). Long inputs are split into windows of this size
// so we never truncate content and each call stays under model output limits.
const maxChunkInputRunes = 18000

// Max concurrent LLM calls per job when a document spans multiple windows.
const maxWindowConcurrency = 4

var (
	llmClient     *llm.Client
	llmClientOnce sync.Once
)

func client() *llm.Client {
	llmClientOnce.Do(func() { llmClient = llm.NewFromEnv() })
	return llmClient
}

// chunkText splits source text into chunks using the LLM. Long text is windowed
// and each window chunked concurrently (order preserved). With no API key it
// falls back to a single whole-document chunk so the pipeline still works
// offline. The second return is the model used (or a "none…" marker).
func chunkText(ctx context.Context, title, text string) ([]Chunk, string) {
	c := client()
	if !c.Available() {
		return []Chunk{fallbackChunk(title, text)}, "none"
	}

	windows := windowText(text, maxChunkInputRunes)
	if len(windows) == 0 {
		return []Chunk{fallbackChunk(title, text)}, "none"
	}

	chunks, anyOK, lastErr := chunkWindows(ctx, c, title, windows)
	if !anyOK {
		// Every window failed — file the whole thing raw rather than lose it.
		return []Chunk{fallbackChunk(title, text)}, "none (llm error: " + errStr(lastErr) + ")"
	}
	return chunks, c.Model()
}

// chunkWindows chunks each window concurrently (bounded) and merges the results
// in original order. A window that fails is preserved as a raw fallback chunk.
func chunkWindows(ctx context.Context, c *llm.Client, title string, windows []string) (all []Chunk, anyOK bool, lastErr error) {
	type result struct {
		chunks []Chunk
		ok     bool
		err    error
	}
	results := make([]result, len(windows))

	sem := make(chan struct{}, maxWindowConcurrency)
	var wg sync.WaitGroup
	for i, w := range windows {
		wg.Add(1)
		go func(i int, w string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			chunks, err := llmChunk(ctx, c, title, w)
			if err != nil || len(chunks) == 0 {
				results[i] = result{
					chunks: []Chunk{{Title: windowFallbackTitle(title, i), Body: w}},
					err:    err,
				}
				return
			}
			results[i] = result{chunks: chunks, ok: true}
		}(i, w)
	}
	wg.Wait()

	for _, r := range results {
		all = append(all, r.chunks...)
		if r.ok {
			anyOK = true
		} else if r.err != nil {
			lastErr = r.err
		}
	}
	return all, anyOK, lastErr
}

func llmChunk(ctx context.Context, c *llm.Client, title, text string) ([]Chunk, error) {
	msgs := []llm.Message{
		{Role: "system", Content: chunkSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("Title: %s\n\n---\n%s", title, text)},
	}
	raw, err := c.Complete(ctx, msgs, true)
	if err != nil {
		return nil, err
	}
	return parseChunks(raw)
}

// windowText splits text into windows of at most maxRunes, breaking on line
// boundaries so chunks land on natural breaks. A single line longer than
// maxRunes is hard-split.
func windowText(text string, maxRunes int) []string {
	lines := strings.Split(text, "\n")
	var windows []string
	var cur strings.Builder
	curLen := 0

	flush := func() {
		if cur.Len() > 0 {
			windows = append(windows, strings.TrimSpace(cur.String()))
			cur.Reset()
			curLen = 0
		}
	}

	for _, line := range lines {
		ln := []rune(line)
		// Hard-split any single line that exceeds the window on its own.
		for len(ln) > maxRunes {
			flush()
			windows = append(windows, string(ln[:maxRunes]))
			ln = ln[maxRunes:]
		}
		add := len(ln) + 1 // +1 for the newline
		if curLen+add > maxRunes && curLen > 0 {
			flush()
		}
		cur.WriteString(string(ln))
		cur.WriteByte('\n')
		curLen += add
	}
	flush()
	return windows
}

func windowFallbackTitle(title string, i int) string {
	base := strings.TrimSpace(title)
	if base == "" {
		base = "Section"
	}
	return fmt.Sprintf("%s (part %d)", base, i+1)
}

const chunkSystemPrompt = `You split source documents into self-contained, topically-coherent chunks for a personal knowledge base.

Rules:
- Each chunk must stand alone and be understandable without the others.
- Preserve technical detail, code, numbers, and names. Do not invent content.
- Split on genuine topic shifts, not arbitrarily. A short document may be a single chunk.
- "body" should be the relevant excerpt, lightly cleaned (fix broken whitespace), not a paraphrase.

Respond with ONLY a JSON object of this exact shape:
{"chunks":[{"title":"short title","summary":"1-2 sentence summary","tags":["lowercase","keywords"],"body":"the chunk text"}]}`

func parseChunks(raw string) ([]Chunk, error) {
	raw = extractJSON(raw)

	var wrapper struct {
		Chunks []Chunk `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err == nil && len(wrapper.Chunks) > 0 {
		return cleanChunks(wrapper.Chunks), nil
	}
	// Some models return a bare array instead of the {"chunks":[...]} wrapper.
	var arr []Chunk
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return cleanChunks(arr), nil
	}
	return nil, fmt.Errorf("could not parse chunks from model output")
}

func cleanChunks(in []Chunk) []Chunk {
	out := make([]Chunk, 0, len(in))
	for _, c := range in {
		c.Title = strings.TrimSpace(c.Title)
		c.Body = strings.TrimSpace(c.Body)
		if c.Body == "" {
			continue
		}
		if c.Title == "" {
			c.Title = "Untitled"
		}
		out = append(out, c)
	}
	return out
}

// extractJSON pulls the outermost {...} or [...] block out of a model response
// that may be wrapped in prose or ```json fences.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return s
	}
	var last int
	if s[start] == '{' {
		last = strings.LastIndex(s, "}")
	} else {
		last = strings.LastIndex(s, "]")
	}
	if last > start {
		return s[start : last+1]
	}
	return s
}

func fallbackChunk(title, text string) Chunk {
	if title == "" {
		title = "Untitled"
	}
	return Chunk{Title: title, Body: text}
}

func errStr(err error) string {
	if err == nil {
		return "empty output"
	}
	return err.Error()
}
