package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"secondbrain-server/internal/llm"
)

type youtubeInfo struct {
	Title      string
	Uploader   string
	Transcript string
	URL        string
}

// fetchYouTube uses yt-dlp to pull the video's metadata and English captions
// (manual or auto-generated), then parses the VTT into clean transcript text.
func fetchYouTube(ctx context.Context, url string) (youtubeInfo, error) {
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		return youtubeInfo{}, fmt.Errorf("yt-dlp is not installed (needed for youtube ingestion)")
	}

	tmp, err := os.MkdirTemp("", "sb-yt-")
	if err != nil {
		return youtubeInfo{}, err
	}
	defer os.RemoveAll(tmp)

	base := filepath.Join(tmp, "v")
	cmd := exec.CommandContext(ctx, "yt-dlp",
		"--skip-download",
		// We only ever want captions + metadata, never the media stream. When
		// YouTube offers no downloadable format (e.g. "Only images are
		// available"), don't let format selection fail the whole run — the
		// subtitles we're after can still be written.
		"--ignore-no-formats-error",
		"--write-info-json",
		"--write-auto-subs", "--write-subs",
		"--sub-langs", "en.*,en",
		"--sub-format", "vtt",
		"-o", base+".%(ext)s",
		url,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return youtubeInfo{}, fmt.Errorf("yt-dlp failed: %v: %s", err, tail(string(out), 300))
	}

	info := youtubeInfo{URL: url}

	if data, err := os.ReadFile(base + ".info.json"); err == nil {
		var meta struct {
			Title    string `json:"title"`
			Uploader string `json:"uploader"`
			Channel  string `json:"channel"`
		}
		if json.Unmarshal(data, &meta) == nil {
			info.Title = meta.Title
			info.Uploader = firstNonEmpty(meta.Uploader, meta.Channel)
		}
	}

	matches, _ := filepath.Glob(base + "*.vtt")
	if len(matches) == 0 {
		// No captions on this video — fall back to transcribing the audio so
		// any video is capturable, not just ones YouTube has captions for.
		text, err := transcribeAudio(ctx, url, base)
		if err != nil {
			return youtubeInfo{}, fmt.Errorf("no captions available; %w", err)
		}
		info.Transcript = text
	} else {
		data, err := os.ReadFile(matches[0])
		if err != nil {
			return youtubeInfo{}, err
		}
		info.Transcript = captionsToText(string(data))
	}
	if strings.TrimSpace(info.Transcript) == "" {
		return youtubeInfo{}, fmt.Errorf("transcript was empty after parsing")
	}
	if info.Title == "" {
		info.Title = "youtube video"
	}
	return info, nil
}

var (
	transcriber     *llm.Transcriber
	transcriberOnce sync.Once
)

func asr() *llm.Transcriber {
	transcriberOnce.Do(func() { transcriber = llm.NewTranscriberFromEnv() })
	return transcriber
}

// transcribeAudio is the no-captions fallback: download the video's audio,
// compressed to 16kHz mono so even long talks stay under the transcription
// size limit, then run it through the ASR provider. base is the same temp
// path prefix the caption pass used, so this reuses that scratch directory.
func transcribeAudio(ctx context.Context, url, base string) (string, error) {
	t := asr()
	if !t.Available() {
		return "", fmt.Errorf("audio transcription is not configured (set GROQ_API_KEY)")
	}

	// bestaudio only (no video), re-encoded by ffmpeg to mono 16kHz 32kbps MP3
	// — plenty for speech recognition and ~14MB/hour, so ~1.5h fits the 25MB cap.
	cmd := exec.CommandContext(ctx, "yt-dlp",
		"-f", "bestaudio/best",
		"--extract-audio",
		"--audio-format", "mp3",
		"--postprocessor-args", "ffmpeg:-ac 1 -ar 16000 -b:a 32k",
		"-o", base+".%(ext)s",
		url,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("audio download failed: %v: %s", err, tail(string(out), 300))
	}

	audio := base + ".mp3"
	if _, err := os.Stat(audio); err != nil {
		return "", fmt.Errorf("audio file missing after download")
	}
	text, err := t.Transcribe(ctx, audio)
	if err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}
	return text, nil
}

var captionTagRe = regexp.MustCompile(`<[^>]*>`)

// captionsToText flattens a VTT (or SRT) caption file into plain transcript
// text: drop headers/timestamps/index lines, strip inline tags, and collapse
// the consecutive duplicate lines that auto-captions produce as they scroll.
func captionsToText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	prev := ""
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || t == "WEBVTT" ||
			strings.HasPrefix(t, "Kind:") ||
			strings.HasPrefix(t, "Language:") ||
			strings.HasPrefix(t, "NOTE") ||
			strings.Contains(t, "-->") ||
			isAllDigits(t) {
			continue
		}
		t = strings.TrimSpace(captionTagRe.ReplaceAllString(t, ""))
		t = strings.TrimSpace(html.UnescapeString(t))
		if t == "" || t == prev {
			continue
		}
		out = append(out, t)
		prev = t
	}
	return strings.Join(out, "\n")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
