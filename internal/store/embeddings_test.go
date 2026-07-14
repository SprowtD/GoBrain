package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestEncodeDecodeVecRoundtrip(t *testing.T) {
	in := []float32{0, 1, -1, 3.14159, 1e-7, -2.5}
	out := decodeVec(encodeVec(in))
	if !reflect.DeepEqual(in, out) {
		t.Errorf("roundtrip mismatch:\n in = %v\nout = %v", in, out)
	}
	if len(encodeVec(in)) != len(in)*4 {
		t.Errorf("encoded length = %d, want %d", len(encodeVec(in)), len(in)*4)
	}
}

func TestEmbeddingStoreRoundtrip(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	vec := []float32{0.1, 0.2, 0.3}
	if err := UpsertEmbedding("a.md", "hash1", vec); err != nil {
		t.Fatal(err)
	}

	got, err := GetEmbedding("a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != "hash1" || !reflect.DeepEqual(got.Vec, vec) {
		t.Errorf("GetEmbedding = %+v", got)
	}

	// Upsert replaces on conflict.
	if err := UpsertEmbedding("a.md", "hash2", []float32{1, 1, 1}); err != nil {
		t.Fatal(err)
	}
	hashes, err := EmbeddingHashes()
	if err != nil {
		t.Fatal(err)
	}
	if hashes["a.md"] != "hash2" {
		t.Errorf("hash after upsert = %q, want hash2", hashes["a.md"])
	}

	// Missing note -> ErrNotFound.
	if _, err := GetEmbedding("missing.md"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Delete removes it.
	if err := DeleteEmbedding("a.md"); err != nil {
		t.Fatal(err)
	}
	if list, _ := ListEmbeddings(); len(list) != 0 {
		t.Errorf("expected empty after delete, got %d", len(list))
	}
}
