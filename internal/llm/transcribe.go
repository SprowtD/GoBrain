package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Transcriber turns an audio file into text via an OpenAI-compatible
// /audio/transcriptions endpoint (Whisper). It's separate from Client because
// OpenRouter has no audio API — Groq is the default (cheap, fast, same shape),
// overridable for OpenAI or any compatible provider.
type Transcriber struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewTranscriberFromEnv reads TRANSCRIBE_API_KEY (falling back to GROQ_API_KEY),
// TRANSCRIBE_MODEL, and TRANSCRIBE_BASE_URL. Defaults target Groq Whisper.
func NewTranscriberFromEnv() *Transcriber {
	key := os.Getenv("TRANSCRIBE_API_KEY")
	if key == "" {
		key = os.Getenv("GROQ_API_KEY")
	}
	return &Transcriber{
		apiKey:  key,
		model:   envOr("TRANSCRIBE_MODEL", "whisper-large-v3-turbo"),
		baseURL: strings.TrimRight(envOr("TRANSCRIBE_BASE_URL", "https://api.groq.com/openai/v1"), "/"),
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

func (t *Transcriber) Available() bool { return t.apiKey != "" }
func (t *Transcriber) Model() string   { return t.model }

// MaxAudioBytes is the provider's upload cap (Groq/OpenAI reject audio over
// 25MB). Callers compress audio before calling; this is the pre-flight guard.
const MaxAudioBytes = 25 << 20

// Transcribe uploads an audio file and returns its plain-text transcription.
func (t *Transcriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if fi, err := f.Stat(); err == nil && fi.Size() > MaxAudioBytes {
		return "", fmt.Errorf("audio is %dMB, over the %dMB transcription limit", fi.Size()>>20, MaxAudioBytes>>20)
	}

	// Build the multipart body: the audio file plus model + response_format.
	// response_format=text makes the endpoint return the raw transcript as the
	// body rather than a JSON envelope, so there's nothing to unwrap.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", t.model)
	_ = mw.WriteField("response_format", "text")
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}
