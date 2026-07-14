package ingest

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"strings"
	"time"
)

const imageSystemPrompt = `You are an OCR and image-understanding assistant.
Transcribe ALL text visible in the image verbatim, preserving reading order and structure.
Then, if there is meaningful non-text content (a diagram, chart, screenshot layout, or photo), briefly describe it.
Output plain text only — no preamble, no markdown fences.`

const imageUserPrompt = "Extract the text from this image and describe it."

var errSVGUnsupported = fmt.Errorf("SVG images aren't supported by vision models; use PNG, JPEG, or WEBP")

type imagePayload struct {
	ref   string // the value to hand the vision model (URL or data: URL)
	isURL bool   // true only for http(s) URLs (safe to store as `resource`)
}

// normalizeImagePayload accepts an http(s) image URL or a data: URL. SVG is
// rejected up front — vision models only take raster formats (PNG/JPEG/WEBP/GIF).
func normalizeImagePayload(payload string) (imagePayload, error) {
	p := strings.TrimSpace(payload)
	if strings.HasPrefix(p, "data:") {
		if strings.HasPrefix(strings.ToLower(p), "data:image/svg") {
			return imagePayload{}, errSVGUnsupported
		}
		return imagePayload{ref: p, isURL: false}, nil
	}
	if u, err := nurl.Parse(p); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		if strings.HasSuffix(strings.ToLower(u.Path), ".svg") {
			return imagePayload{}, errSVGUnsupported
		}
		return imagePayload{ref: p, isURL: true}, nil
	}
	return imagePayload{}, fmt.Errorf("image payload must be an http(s) URL or a data: URL")
}

var imageHTTP = &http.Client{Timeout: 20 * time.Second}

// loadImageBytes returns the raw image bytes and a file extension so the source
// image can be stored in the vault and embedded in the note.
func loadImageBytes(ctx context.Context, img imagePayload) ([]byte, string, error) {
	if strings.HasPrefix(img.ref, "data:") {
		return decodeDataURL(img.ref)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.ref, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "secondbrain/1.0")
	resp, err := imageHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image download status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 15<<20))
	if err != nil {
		return nil, "", err
	}
	ext := firstNonEmpty(extFromContentType(resp.Header.Get("Content-Type")), extFromURLPath(img.ref), "png")
	return data, ext, nil
}

func decodeDataURL(u string) ([]byte, string, error) {
	comma := strings.IndexByte(u, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("malformed data url")
	}
	meta := u[len("data:"):comma]
	payload := u[comma+1:]

	mime := meta
	if i := strings.IndexByte(meta, ';'); i >= 0 {
		mime = meta[:i]
	}
	if !strings.Contains(meta, "base64") {
		return nil, "", fmt.Errorf("only base64 data urls are supported")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", fmt.Errorf("decode data url: %w", err)
	}
	return data, firstNonEmpty(extFromMime(mime), "png"), nil
}

func extFromMime(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return ""
}

func extFromContentType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return extFromMime(ct)
}

func extFromURLPath(u string) string {
	pu, err := nurl.Parse(u)
	if err != nil {
		return ""
	}
	p := strings.ToLower(pu.Path)
	for _, e := range []string{"png", "jpg", "jpeg", "webp", "gif"} {
		if strings.HasSuffix(p, "."+e) {
			if e == "jpeg" {
				return "jpg"
			}
			return e
		}
	}
	return ""
}
