package ingest

import "testing"

func TestCaptionsToText(t *testing.T) {
	// Auto-caption VTT: headers, timestamps, inline tags, and the rolling
	// duplicate lines that scrolling captions produce.
	vtt := "WEBVTT\n" +
		"Kind: captions\n" +
		"Language: en\n" +
		"\n" +
		"00:00:00.000 --> 00:00:02.000 align:start position:0%\n" +
		"hello<00:00:00.480><c> world</c>\n" +
		"\n" +
		"00:00:02.000 --> 00:00:04.000 align:start position:0%\n" +
		"hello world\n" +
		"this is a &amp; test\n"

	got := captionsToText(vtt)
	want := "hello world\nthis is a & test"
	if got != want {
		t.Fatalf("captionsToText:\n got: %q\nwant: %q", got, want)
	}
}

func TestCaptionsToTextSRT(t *testing.T) {
	srt := "1\n00:00:00,000 --> 00:00:02,000\nfirst line\n\n2\n00:00:02,000 --> 00:00:04,000\nsecond line\n"
	got := captionsToText(srt)
	want := "first line\nsecond line"
	if got != want {
		t.Fatalf("srt: got %q want %q", got, want)
	}
}

func TestNormalizeImagePayload(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		isURL   bool
	}{
		{"https://example.com/a.png", false, true},
		{"http://example.com/a.jpg", false, true},
		{"data:image/png;base64,iVBORw0KGgo=", false, false},
		{"not a url", true, false},
		{"ftp://example.com/a.png", true, false},
		{"", true, false},
	}
	for _, tc := range cases {
		got, err := normalizeImagePayload(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeImagePayload(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeImagePayload(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.isURL != tc.isURL {
			t.Errorf("normalizeImagePayload(%q): isURL=%v want %v", tc.in, got.isURL, tc.isURL)
		}
	}
}
