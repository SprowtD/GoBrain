package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
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
	args := []string{
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
		"-o", base + ".%(ext)s",
	}
	args = append(args, ytdlpNetArgs()...)
	args = append(args, url)
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
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
		// The overwhelming majority of videos have captions (YouTube
		// auto-generates them), so by default we just report the rare miss
		// cleanly. Transcribing the audio instead is possible but needs an ASR
		// key and — because pulling media trips YouTube's datacenter bot wall —
		// cookies, which doesn't suit a share-and-forget mobile flow. It stays
		// available, opt-in, behind YOUTUBE_AUDIO_FALLBACK for anyone who wants it.
		if !audioFallbackEnabled() {
			return youtubeInfo{}, fmt.Errorf("this video has no captions available")
		}
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

// audioFallbackEnabled gates the no-captions audio-transcription path. Off by
// default (it needs an ASR key + cookies, unsuited to the mobile flow); set
// YOUTUBE_AUDIO_FALLBACK=true to revive it.
func audioFallbackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("YOUTUBE_AUDIO_FALLBACK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ytdlpNetArgs are the extra flags that get yt-dlp past YouTube's datacenter-IP
// blocking: a residential proxy and/or session cookies. YouTube flags cloud IPs
// (like Railway's) with a "confirm you're not a bot" wall — often only after a
// burst of requests, which is why direct access works a few times then stops.
// Both are optional and off by default; applied to every yt-dlp call.
func ytdlpNetArgs() []string {
	return append(ytdlpCookieArgs(), ytdlpProxyArgs()...)
}

// ytdlpProxyArgs routes yt-dlp through a proxy when YTDLP_PROXY is set (e.g.
// http://user:pass@host:port or socks5://…). Use a *residential* proxy —
// datacenter proxies are blocked by YouTube just like Railway's own IP. Captions
// are tiny, so pay-as-you-go residential bandwidth costs fractions of a cent per
// video.
func ytdlpProxyArgs() []string {
	if p := strings.TrimSpace(os.Getenv("YTDLP_PROXY")); p != "" {
		return []string{"--proxy", p}
	}
	return nil
}

// ytdlpCookieArgs supplies cookies from a logged-in session — the free
// alternative to a proxy. Set YTDLP_COOKIES_FILE to a path, or paste a Netscape
// cookies.txt into YTDLP_COOKIES and we write it to disk once. Nil when unset.
func ytdlpCookieArgs() []string {
	if p := strings.TrimSpace(os.Getenv("YTDLP_COOKIES_FILE")); p != "" {
		return []string{"--cookies", p}
	}
	if p := cookieFileFromEnv(); p != "" {
		return []string{"--cookies", p}
	}
	return nil
}

var (
	cookieFilePath string
	cookieFileOnce sync.Once
)

func cookieFileFromEnv() string {
	cookieFileOnce.Do(func() {
		contents := os.Getenv("YTDLP_COOKIES")
		if strings.TrimSpace(contents) == "" {
			return
		}
		p := filepath.Join(os.TempDir(), "yt-cookies.txt")
		if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
			log.Printf("youtube: could not write YTDLP_COOKIES to disk: %v", err)
			return
		}
		cookieFilePath = p
	})
	return cookieFilePath
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
	args := []string{
		"-f", "bestaudio/best",
		"--extract-audio",
		"--audio-format", "mp3",
		"--postprocessor-args", "ffmpeg:-ac 1 -ar 16000 -b:a 32k",
		"-o", base + ".%(ext)s",
	}
	args = append(args, ytdlpNetArgs()...)
	args = append(args, url)
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
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
