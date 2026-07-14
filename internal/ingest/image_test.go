package ingest

import "testing"

// 1x1 transparent PNG.
const onePxPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func TestDecodeDataURL(t *testing.T) {
	data, ext, err := decodeDataURL("data:image/png;base64," + onePxPNG)
	if err != nil {
		t.Fatalf("decodeDataURL: %v", err)
	}
	if ext != "png" {
		t.Errorf("ext = %q, want png", ext)
	}
	if len(data) == 0 || string(data[1:4]) != "PNG" {
		t.Errorf("decoded bytes don't look like a PNG: %d bytes", len(data))
	}
}

func TestDecodeDataURLRejectsNonBase64(t *testing.T) {
	if _, _, err := decodeDataURL("data:image/png,notbase64"); err == nil {
		t.Error("expected error for non-base64 data url")
	}
}

func TestExtHelpers(t *testing.T) {
	cases := map[string]string{
		"image/png":                "png",
		"image/jpeg":               "jpg",
		"image/webp":               "webp",
		"image/gif":                "gif",
		"application/octet-stream": "",
	}
	for mime, want := range cases {
		if got := extFromMime(mime); got != want {
			t.Errorf("extFromMime(%q)=%q want %q", mime, got, want)
		}
	}
	if got := extFromContentType("image/jpeg; charset=binary"); got != "jpg" {
		t.Errorf("extFromContentType with params = %q, want jpg", got)
	}
	if got := extFromURLPath("https://x.com/a/b/photo.JPG?q=1"); got != "jpg" {
		t.Errorf("extFromURLPath = %q, want jpg", got)
	}
}
