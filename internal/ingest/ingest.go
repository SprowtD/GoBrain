package ingest

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"secondbrain-server/internal/store"
	"secondbrain-server/internal/vault"
)

// outFile is a rendered chunk plus the vault-relative path it should be written to.
type outFile struct {
	RelPath string
	Content string
}

// ProcessJob is the worker entrypoint: fetch/extract → chunk → write to vault.
func ProcessJob(job store.Job) {
	short := shortID(job.ID)
	log.Printf("ingest %s [%s]: start %s", short, job.SourceKind, previewPayload(job.Payload))

	// Generous ceiling: long media windows into several (concurrent) LLM calls.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	_ = store.UpdateJobStatus(job.ID, "reading", "")

	files, err := build(ctx, job)
	if err != nil {
		log.Printf("ingest %s [%s]: ✗ FAILED: %v", short, job.SourceKind, err)
		_ = store.UpdateJobStatus(job.ID, "misfiled", err.Error())
		return
	}
	if len(files) == 0 {
		log.Printf("ingest %s [%s]: ✗ FAILED: no content produced", short, job.SourceKind)
		_ = store.UpdateJobStatus(job.ID, "misfiled", "no content produced")
		return
	}

	for _, f := range files {
		vault.Write(vault.WriteRequest{RelPath: f.RelPath, Content: f.Content})
	}

	// Point the job at its first file so /status links somewhere openable.
	if err := store.CompleteJob(job.ID, files[0].RelPath); err != nil {
		log.Printf("ingest %s [%s]: ✗ FAILED (status write): %v", short, job.SourceKind, err)
		_ = store.UpdateJobStatus(job.ID, "misfiled", err.Error())
		return
	}

	log.Printf("ingest %s [%s]: ✓ filed %d file(s) → %s", short, job.SourceKind, len(files), files[0].RelPath)
}

// previewPayload trims a payload for logging (data URLs are huge; URLs are long).
func previewPayload(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "data:") {
		return "<data url>"
	}
	if len(p) > 70 {
		return p[:70] + "…"
	}
	return p
}

func build(ctx context.Context, job store.Job) ([]outFile, error) {
	switch job.SourceKind {
	case "article":
		return buildArticle(ctx, job)
	case "youtube":
		return buildYouTube(ctx, job)
	case "image":
		return buildImage(ctx, job)
	case "thought":
		return buildThought(job), nil
	default:
		return nil, fmt.Errorf("unsupported source_kind %q", job.SourceKind)
	}
}

func buildArticle(ctx context.Context, job store.Job) ([]outFile, error) {
	art, err := fetchArticle(ctx, job.Payload)
	if err != nil {
		return nil, err
	}
	chunks, model := chunkText(ctx, art.Title, art.Text)
	return renderChunksToFiles(job, art.Title, art.URL, art.Site, art.Byline, model, chunks), nil
}

func buildYouTube(ctx context.Context, job store.Job) ([]outFile, error) {
	yt, err := fetchYouTube(ctx, job.Payload)
	if err != nil {
		return nil, err
	}
	chunks, model := chunkText(ctx, yt.Title, yt.Transcript)
	return renderChunksToFiles(job, yt.Title, yt.URL, "YouTube", yt.Uploader, model, chunks), nil
}

func buildImage(ctx context.Context, job store.Job) ([]outFile, error) {
	img, err := normalizeImagePayload(job.Payload)
	if err != nil {
		return nil, err
	}
	c := client()
	if !c.Available() {
		return nil, fmt.Errorf("image ingestion needs OPENROUTER_API_KEY (vision model)")
	}

	// iPhones shoot HEIC by default, which vision models reject; transcode
	// those to an inline JPEG data URL (jpg carries the bytes to the vault).
	visionRef, jpg, err := prepareVisionImage(ctx, img)
	if err != nil {
		return nil, fmt.Errorf("prepare image: %w", err)
	}

	raw, err := c.CompleteVision(ctx, imageSystemPrompt, imageUserPrompt, visionRef)
	if err != nil {
		return nil, fmt.Errorf("vision model: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("vision model returned no text")
	}

	chunks, chunkModel := chunkText(ctx, "", raw)
	title := "image"
	if len(chunks) > 0 && chunks[0].Title != "" {
		title = chunks[0].Title
	}
	// Only store the URL as `resource`; never inline a giant data: URL.
	resource := ""
	if img.isURL {
		resource = job.Payload
	}
	model := c.VisionModel() + " → " + chunkModel

	// Save the source image into the vault and embed it at the top of the note
	// so Obsidian shows the picture alongside the transcription.
	asset := storeImageAsset(ctx, job, title, img, chunks, jpg)

	files := renderChunksToFiles(job, title, resource, "", "", model, chunks)
	if asset != nil {
		files = append(files, *asset)
	}
	return files, nil
}

// storeImageAsset downloads/decodes the source image, prepends an embed to the
// first chunk's body, and returns the asset file to write (nil if unavailable).
// It mutates chunks[0].Body to include the embed.
// converted holds JPEG bytes from a HEIC transcode (nil when no conversion
// happened); when present it's embedded directly instead of re-fetching.
func storeImageAsset(ctx context.Context, job store.Job, title string, img imagePayload, chunks []Chunk, converted []byte) *outFile {
	if len(chunks) == 0 {
		return nil
	}
	data, ext := converted, "jpg"
	if data == nil {
		var err error
		data, ext, err = loadImageBytes(ctx, img)
		if err != nil || len(data) == 0 {
			if err != nil {
				log.Printf("ingest %s [image]: storing image failed (%v); note is text-only", shortID(job.ID), err)
			}
			// Fall back to embedding the remote URL so it at least renders online.
			if img.isURL {
				chunks[0].Body = "![](" + job.Payload + ")\n\n" + chunks[0].Body
			}
			return nil
		}
	}

	short := shortID(job.ID)
	itemSlug := slugify(title)
	var embedName, assetRel string
	if len(chunks) > 1 {
		embedName = "image." + ext
		assetRel = filepath.Join(job.SourceKind, itemSlug+"-"+short, embedName)
	} else {
		embedName = itemSlug + "-" + short + "." + ext
		assetRel = filepath.Join(job.SourceKind, embedName)
	}

	// The asset sits in the same directory as the note, so a bare filename embed resolves.
	chunks[0].Body = "![](" + embedName + ")\n\n" + chunks[0].Body
	return &outFile{RelPath: assetRel, Content: string(data)}
}

func buildThought(job store.Job) []outFile {
	title := firstLine(job.Payload)
	content := renderChunk(fileMeta{
		Type:       okfType(job.SourceKind),
		Timestamp:  nowRFC3339(),
		JobID:      job.ID,
		SourceKind: job.SourceKind,
		Note:       job.Note,
		Model:      "none",
	}, Chunk{Title: title, Body: job.Payload})

	rel := filepath.Join(job.SourceKind, slugify(title)+"-"+shortID(job.ID)+".md")
	return []outFile{{RelPath: rel, Content: content}}
}

// renderChunksToFiles turns chunks into vault files with OKF frontmatter and
// readable, unique paths: multi-chunk sources get their own folder with ordered
// files; single-chunk sources are one file. Shared by article/youtube/image.
func renderChunksToFiles(job store.Job, itemTitle, resource, site, byline, model string, chunks []Chunk) []outFile {
	now := nowRFC3339()
	short := shortID(job.ID)
	itemSlug := slugify(itemTitle)
	multi := len(chunks) > 1

	out := make([]outFile, 0, len(chunks))
	for i, c := range chunks {
		content := renderChunk(fileMeta{
			Type:       okfType(job.SourceKind),
			Resource:   resource,
			Timestamp:  now,
			JobID:      job.ID,
			SourceKind: job.SourceKind,
			Site:       site,
			Byline:     byline,
			Note:       job.Note,
			Model:      model,
		}, c)

		var rel string
		if multi {
			rel = filepath.Join(job.SourceKind, itemSlug+"-"+short,
				fmt.Sprintf("%02d-%s.md", i+1, slugify(c.Title)))
		} else {
			rel = filepath.Join(job.SourceKind, itemSlug+"-"+short+".md")
		}
		out = append(out, outFile{RelPath: rel, Content: content})
	}
	return out
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
