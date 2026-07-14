package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
)

type article struct {
	URL    string
	Title  string
	Byline string
	Site   string
	Text   string
}

var articleClient = &http.Client{Timeout: 20 * time.Second}

// fetchArticle downloads a URL and extracts its main readable text via a
// Readability port. Only http/https are allowed.
func fetchArticle(ctx context.Context, rawURL string) (article, error) {
	u, err := nurl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return article{}, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return article{}, fmt.Errorf("unsupported url scheme %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return article{}, err
	}
	req.Header.Set("User-Agent", "secondbrain/1.0 (+https://github.com/secondbrain)")

	resp, err := articleClient.Do(req)
	if err != nil {
		return article{}, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return article{}, fmt.Errorf("fetch: unexpected status %d", resp.StatusCode)
	}

	// Cap the body so a giant page can't blow up memory.
	art, err := readability.FromReader(io.LimitReader(resp.Body, 5<<20), u)
	if err != nil {
		return article{}, fmt.Errorf("readability: %w", err)
	}

	var buf bytes.Buffer
	if err := art.RenderText(&buf); err != nil {
		return article{}, fmt.Errorf("render text: %w", err)
	}
	text := strings.TrimSpace(buf.String())
	if text == "" {
		return article{}, fmt.Errorf("no readable content extracted")
	}

	return article{
		URL:    rawURL,
		Title:  strings.TrimSpace(art.Title()),
		Byline: strings.TrimSpace(art.Byline()),
		Site:   strings.TrimSpace(art.SiteName()),
		Text:   text,
	}, nil
}
