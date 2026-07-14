// Package llm is a thin OpenRouter chat client. OpenRouter exposes an
// OpenAI-compatible API, so switching models is just OPENROUTER_MODEL /
// OPENROUTER_VISION_MODEL.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Client struct {
	apiKey         string
	model          string
	visionModel    string
	embeddingModel string
	baseURL        string
	http           *http.Client
}

// NewFromEnv reads OPENROUTER_API_KEY, OPENROUTER_MODEL,
// OPENROUTER_VISION_MODEL, OPENROUTER_EMBEDDING_MODEL, OPENROUTER_BASE_URL.
func NewFromEnv() *Client {
	return &Client{
		apiKey:         os.Getenv("OPENROUTER_API_KEY"),
		model:          envOr("OPENROUTER_MODEL", "anthropic/claude-3.5-sonnet"),
		visionModel:    envOr("OPENROUTER_VISION_MODEL", "openai/gpt-4o-mini"),
		embeddingModel: envOr("OPENROUTER_EMBEDDING_MODEL", "qwen/qwen3-embedding-8b"),
		baseURL:        strings.TrimRight(envOr("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"), "/"),
		http:           &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *Client) Available() bool        { return c.apiKey != "" }
func (c *Client) Model() string          { return c.model }
func (c *Client) VisionModel() string    { return c.visionModel }
func (c *Client) EmbeddingModel() string { return c.embeddingModel }

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// --- multimodal (vision) request shapes ---

type visionRequest struct {
	Model       string          `json:"model"`
	Messages    []visionMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
}

type visionMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []visionPart for multimodal
}

type visionPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageRef `json:"image_url,omitempty"`
}

type imageRef struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a text chat completion. When jsonMode is true it asks the
// model for a JSON object response (honored by most OpenRouter models).
func (c *Client) Complete(ctx context.Context, messages []Message, jsonMode bool) (string, error) {
	reqBody := chatRequest{Model: c.model, Messages: messages, Temperature: 0.2}
	if jsonMode {
		reqBody.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	return c.post(ctx, reqBody)
}

// CompleteVision sends an image (http(s) URL or data: URL) to the configured
// vision model and returns its text output.
func (c *Client) CompleteVision(ctx context.Context, systemPrompt, userText, imageURL string) (string, error) {
	reqBody := visionRequest{
		Model:       c.visionModel,
		Temperature: 0.2,
		Messages: []visionMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: []visionPart{
				{Type: "text", Text: userText},
				{Type: "image_url", ImageURL: &imageRef{URL: imageURL}},
			}},
		},
	}
	return c.post(ctx, reqBody)
}

// post sends any request body to /chat/completions and returns the first
// choice's message content.
func (c *Client) post(ctx context.Context, reqBody any) (string, error) {
	body, err := c.doRequest(ctx, "/chat/completions", reqBody, 4<<20)
	if err != nil {
		return "", err
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("openrouter error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("openrouter returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed returns the embedding vector for a single input string from the
// configured embedding model (OpenRouter's /embeddings endpoint).
func (c *Client) Embed(ctx context.Context, input string) ([]float32, error) {
	body, err := c.doRequest(ctx, "/embeddings",
		embeddingRequest{Model: c.embeddingModel, Input: input}, 8<<20)
	if err != nil {
		return nil, err
	}
	var er embeddingResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return nil, fmt.Errorf("decode embedding: %w", err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("openrouter error: %s", er.Error.Message)
	}
	if len(er.Data) == 0 || len(er.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("openrouter returned no embedding")
	}
	return er.Data[0].Embedding, nil
}

// doRequest marshals a body, POSTs it to the given API path, and returns the raw
// response bytes (validating the HTTP status). Shared by chat and embeddings.
func (c *Client) doRequest(ctx context.Context, path string, reqBody any, limit int64) ([]byte, error) {
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	// Optional OpenRouter attribution headers (shown on their dashboard).
	req.Header.Set("HTTP-Referer", envOr("OPENROUTER_REFERER", "https://github.com/secondbrain"))
	req.Header.Set("X-Title", "secondbrain")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
