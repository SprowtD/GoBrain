package store

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
)

// Embedding is one note's stored vector plus the content hash it was computed
// from (so the indexer can skip notes whose text hasn't changed).
type Embedding struct {
	RelPath string
	Hash    string
	Vec     []float32
}

// UpsertEmbedding stores (or replaces) a note's embedding vector.
func UpsertEmbedding(relPath, hash string, vec []float32) error {
	_, err := db.Exec(
		`INSERT INTO embeddings (rel_path, hash, dim, vec, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(rel_path) DO UPDATE SET
		   hash = excluded.hash, dim = excluded.dim,
		   vec = excluded.vec, updated_at = CURRENT_TIMESTAMP`,
		relPath, hash, len(vec), encodeVec(vec),
	)
	return err
}

// EmbeddingHashes returns rel_path -> content hash for every stored embedding,
// so the indexer can decide what needs re-embedding without loading vectors.
func EmbeddingHashes() (map[string]string, error) {
	rows, err := db.Query(`SELECT rel_path, hash FROM embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return nil, err
		}
		out[path] = hash
	}
	return out, rows.Err()
}

// ListEmbeddings loads every stored embedding (path + vector) for ranking.
func ListEmbeddings() ([]Embedding, error) {
	rows, err := db.Query(`SELECT rel_path, hash, vec FROM embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Embedding
	for rows.Next() {
		var (
			e   Embedding
			raw []byte
		)
		if err := rows.Scan(&e.RelPath, &e.Hash, &raw); err != nil {
			return nil, err
		}
		e.Vec = decodeVec(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEmbedding returns a single note's vector, or ErrNotFound if not indexed.
func GetEmbedding(relPath string) (Embedding, error) {
	var (
		e   Embedding
		raw []byte
	)
	e.RelPath = relPath
	err := db.QueryRow(`SELECT hash, vec FROM embeddings WHERE rel_path = ?`, relPath).
		Scan(&e.Hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Embedding{}, ErrNotFound
	}
	if err != nil {
		return Embedding{}, err
	}
	e.Vec = decodeVec(raw)
	return e, nil
}

// DeleteEmbedding removes a note's embedding (used when the note is gone).
func DeleteEmbedding(relPath string) error {
	_, err := db.Exec(`DELETE FROM embeddings WHERE rel_path = ?`, relPath)
	return err
}

// encodeVec packs a float32 slice into little-endian bytes for BLOB storage.
func encodeVec(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeVec is the inverse of encodeVec.
func decodeVec(buf []byte) []float32 {
	vec := make([]float32, len(buf)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return vec
}
