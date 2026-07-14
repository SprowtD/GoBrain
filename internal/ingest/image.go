package ingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	nurl "net/url"
	"strings"
	"time"

	"github.com/gen2brain/heic"
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

// prepareVisionImage returns the reference to hand the vision model and, when a
// conversion happened, the JPEG bytes so the vault can embed a viewable copy.
// iPhones shoot HEIC by default, which OpenRouter's vision models reject (and
// Obsidian can't render); those are decoded and re-encoded to an inline JPEG
// data URL. Everything else passes through unchanged (jpg == nil), keeping the
// original ref — a plain http(s) URL or an already-supported data URL.
func prepareVisionImage(ctx context.Context, img imagePayload) (ref string, jpg []byte, err error) {
	// Only pre-load bytes when the source might be HEIF: any data: URL (cheap,
	// already in memory) or an http(s) URL whose path advertises a HEIF
	// extension. This keeps the common non-HEIF http case a single download in
	// the asset step, unchanged from before.
	if !strings.HasPrefix(img.ref, "data:") && !isHEIFURL(img.ref) {
		return img.ref, nil, nil
	}
	data, _, err := loadImageBytes(ctx, img)
	if err != nil {
		// Couldn't pre-load; fall back to the original ref and let the model try.
		return img.ref, nil, nil
	}
	if !isHEIF(data) {
		return img.ref, nil, nil
	}
	converted, err := transcodeHEIFToJPEG(data)
	if err != nil {
		return "", nil, err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(converted), converted, nil
}

// isHEIF reports whether b is a HEIF/HEIC image, by sniffing the ISO-BMFF
// 'ftyp' box brand. Sniffing the bytes is more reliable than trusting a data:
// URL's declared MIME, which mobile clients often set to a generic value.
func isHEIF(b []byte) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	switch string(b[8:12]) {
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1", "heif":
		return true
	}
	return false
}

func isHEIFURL(u string) bool {
	pu, err := nurl.Parse(u)
	if err != nil {
		return false
	}
	p := strings.ToLower(pu.Path)
	return strings.HasSuffix(p, ".heic") || strings.HasSuffix(p, ".heif")
}

// transcodeHEIFToJPEG decodes HEIF/HEIC bytes and re-encodes them as JPEG.
func transcodeHEIFToJPEG(data []byte) ([]byte, error) {
	src, err := heic.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode HEIC: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 88}); err != nil {
		return nil, fmt.Errorf("encode JPEG: %w", err)
	}
	return buf.Bytes(), nil
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
