// Package index builds and queries a semantic (embedding) index over the vault.
// Vectors live in SQLite (derived, per-instance, rebuildable) so the vault
// markdown stays pure. Every query path falls back to vault keyword search when
// embeddings are unconfigured or a call fails, so the feature is purely additive.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"secondbrain-server/internal/llm"
	"secondbrain-server/internal/store"
	"secondbrain-server/internal/vault"
)

const (
	// maxEmbedRunes caps how much of a note we send to the embedding model. Notes
	// are windowed to ~18k runes at ingest; embedding models have a token
	// ceiling, so we truncate to a safe leading slice.
	maxEmbedRunes = 8000
	// relatedCount / relatedMinScore bound the auto-generated Related block:
	// at most N links, and only neighbours above this cosine similarity (below
	// it, notes aren't meaningfully related — avoids linking everything).
	relatedCount    = 5
	relatedMinScore = 0.4
)

var (
	client       *llm.Client
	enabled      bool
	relatedLinks bool
	reindex      sync.Mutex // serialize reconciles so two never race the same rows
)

// Init wires the embedding client. Semantic features are enabled only when the
// client has an API key; otherwise every call degrades to keyword search.
// Auto-injected [[related]] links default on (with embeddings) — set
// RELATED_LINKS=false to disable body mutation.
func Init(c *llm.Client) {
	client = c
	enabled = c != nil && c.Available()
	relatedLinks = enabled && !isFalse(os.Getenv("RELATED_LINKS"))
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

// Enabled reports whether semantic search is active.
func Enabled() bool { return enabled }

// Reconcile brings the embedding index in sync with the vault: embeds new or
// changed notes, and prunes vectors for deleted notes. Safe to call repeatedly
// (it skips notes whose content hash is unchanged) and to run in a goroutine.
func Reconcile(ctx context.Context) error {
	if !enabled {
		return nil
	}
	reindex.Lock()
	defer reindex.Unlock()

	notes, err := vault.ListNotes()
	if err != nil {
		return err
	}
	have, err := store.EmbeddingHashes()
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(notes))
	var embedded int
	for _, n := range notes {
		seen[n.Path] = true
		// Hash/embed the note WITHOUT its managed Related block, so regenerating
		// that block never counts as a content change (which would re-embed
		// endlessly). embedInput strips it too.
		h := hashText(vault.StripManagedBlock(n.Text))
		if have[n.Path] == h {
			continue // unchanged since last index
		}
		vec, err := client.Embed(ctx, embedInput(n))
		if err != nil {
			// Don't fail the whole pass on one note; log and move on. The next
			// reconcile will retry it (its hash still won't match).
			log.Printf("index: embed %s failed: %v", n.Path, err)
			continue
		}
		normalize(vec)
		if err := store.UpsertEmbedding(n.Path, h, vec); err != nil {
			log.Printf("index: store %s failed: %v", n.Path, err)
			continue
		}
		embedded++
	}

	// Prune embeddings whose note no longer exists.
	for path := range have {
		if !seen[path] {
			if err := store.DeleteEmbedding(path); err != nil {
				log.Printf("index: prune %s failed: %v", path, err)
			}
		}
	}
	if embedded > 0 {
		log.Printf("index: embedded %d note(s), %d indexed total", embedded, len(seen))
	}

	// Regenerate the auto [[related]] blocks. Runs after embeddings are current;
	// writes only the notes whose block actually changed, so it converges (no
	// commit loop). Any writes here trigger one more commit → reconcile, which
	// then finds every block already correct and writes nothing.
	if relatedLinks {
		writeRelatedBlocks(notes)
	}
	return nil
}

// writeRelatedBlocks refreshes each note's machine-managed Related section from
// its nearest neighbours (above relatedMinScore).
func writeRelatedBlocks(notes []vault.Note) {
	embs, err := store.ListEmbeddings()
	if err != nil || len(embs) == 0 {
		return
	}
	byPath := make(map[string][]float32, len(embs))
	for _, e := range embs {
		byPath[e.RelPath] = e.Vec
	}

	var wrote int
	for _, n := range notes {
		vec := byPath[n.Path]
		if vec == nil {
			continue // not embedded yet
		}
		var items []vault.RelatedItem
		for _, s := range rank(vec, embs, n.Path, relatedCount) {
			if s.score < relatedMinScore {
				continue
			}
			title, _ := vault.NoteTitle(s.path)
			items = append(items, vault.RelatedItem{Path: s.path, Title: title})
		}
		changed, err := vault.SetRelated(n.Path, items)
		if err != nil {
			log.Printf("index: related block %s failed: %v", n.Path, err)
			continue
		}
		if changed {
			wrote++
		}
	}
	if wrote > 0 {
		log.Printf("index: updated related links on %d note(s)", wrote)
	}
}

// Search ranks notes by semantic similarity to the query, falling back to vault
// keyword search when embeddings are unavailable, the query can't be embedded,
// or nothing is indexed yet.
func Search(ctx context.Context, query string, limit int) ([]vault.SearchHit, error) {
	if limit <= 0 {
		limit = 20
	}
	if !enabled {
		return vault.Search(query, limit)
	}
	qv, err := client.Embed(ctx, query)
	if err != nil {
		log.Printf("index: query embed failed, falling back to keyword: %v", err)
		return vault.Search(query, limit)
	}
	normalize(qv)

	embs, err := store.ListEmbeddings()
	if err != nil || len(embs) == 0 {
		return vault.Search(query, limit)
	}

	ranked := rank(qv, embs, "", limit)
	if len(ranked) == 0 {
		return vault.Search(query, limit)
	}
	return hydrate(ranked), nil
}

// Related returns the notes most semantically similar to the given note. Uses
// the note's stored vector when available, otherwise embeds it on the fly.
// Returns nil (no error) when semantic search is disabled.
func Related(ctx context.Context, path string, limit int) ([]vault.SearchHit, error) {
	if !enabled {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	var qv []float32
	if e, err := store.GetEmbedding(path); err == nil {
		qv = e.Vec // already normalized at store time
	} else {
		text, err := vault.ReadNote(path)
		if err != nil {
			return nil, err
		}
		qv, err = client.Embed(ctx, truncate(text))
		if err != nil {
			return nil, err
		}
		normalize(qv)
	}

	embs, err := store.ListEmbeddings()
	if err != nil {
		return nil, err
	}
	return hydrate(rank(qv, embs, path, limit)), nil
}

// --- ranking ---

type scored struct {
	path  string
	score float64
}

// rank scores every embedding against qv by dot product (vectors are unit-
// normalized, so dot == cosine), skips `exclude` and dimension mismatches, and
// returns the top `limit`.
func rank(qv []float32, embs []store.Embedding, exclude string, limit int) []scored {
	var out []scored
	for _, e := range embs {
		if e.RelPath == exclude || len(e.Vec) != len(qv) {
			continue
		}
		out = append(out, scored{path: e.RelPath, score: dot(qv, e.Vec)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// hydrate turns ranked paths into SearchHits with title + leading excerpt.
func hydrate(ranked []scored) []vault.SearchHit {
	hits := make([]vault.SearchHit, 0, len(ranked))
	for _, s := range ranked {
		text, err := vault.ReadNote(s.path)
		if err != nil {
			continue // note vanished between ranking and read
		}
		title, _ := vault.NoteTitle(s.path)
		hits = append(hits, vault.SearchHit{
			Path:    s.path,
			Title:   title,
			Snippet: vault.Excerpt(text, 200),
			Score:   round(s.score),
		})
	}
	return hits
}

// --- vector math ---

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// normalize scales a vector to unit length in place, so later dot products equal
// cosine similarity. A zero vector is left untouched.
func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// --- helpers ---

func embedInput(n vault.Note) string {
	// Lead with the title so short notes still carry topical signal; strip the
	// managed Related block so link churn never changes the embedding input.
	return truncate(n.Title + "\n\n" + vault.StripManagedBlock(n.Text))
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) > maxEmbedRunes {
		return string(r[:maxEmbedRunes])
	}
	return s
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func round(f float64) float64 {
	return math.Round(f*10000) / 10000
}
