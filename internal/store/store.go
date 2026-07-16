package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a lookup/update targets a row that doesn't exist.
var ErrNotFound = errors.New("not found")

// db is the single shared connection pool. SQLite is capped to one open
// connection to avoid "database is locked" churn under the worker pool.
var db *sql.DB

func Init(path string) error {
	// SQLite does not create the parent directory; ensure it exists.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db dir: %w", err)
		}
	}

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		return fmt.Errorf("set pragmas: %w", err)
	}
	if err := migrate(conn); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	db = conn
	return nil
}

func migrate(conn *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS tokens (
	id          TEXT PRIMARY KEY,
	label       TEXT NOT NULL,
	token_hash  TEXT NOT NULL UNIQUE,
	role        TEXT NOT NULL DEFAULT 'member',
	revoked     INTEGER NOT NULL DEFAULT 0,
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT PRIMARY KEY,
	source_kind  TEXT NOT NULL,
	payload      TEXT NOT NULL,
	note         TEXT,
	status       TEXT NOT NULL DEFAULT 'queued',
	error        TEXT,
	output_path  TEXT,
	token_label  TEXT,
	content_hash TEXT,
	title        TEXT,
	created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at DESC);
CREATE TABLE IF NOT EXISTS embeddings (
	rel_path    TEXT PRIMARY KEY,
	hash        TEXT NOT NULL,
	dim         INTEGER NOT NULL,
	vec         BLOB NOT NULL,
	updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- OAuth 2.1 support for the remote MCP connector (see oauth.go).
CREATE TABLE IF NOT EXISTS oauth_clients (
	client_id     TEXT PRIMARY KEY,
	redirect_uris TEXT NOT NULL,   -- JSON array
	client_name   TEXT,
	created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS oauth_codes (
	code_hash      TEXT PRIMARY KEY,
	client_id      TEXT NOT NULL,
	redirect_uri   TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	label          TEXT NOT NULL,
	role           TEXT NOT NULL,
	expires_at     TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_tokens (
	token_hash   TEXT PRIMARY KEY,
	refresh_hash TEXT,
	client_id    TEXT NOT NULL,
	label        TEXT NOT NULL,
	role         TEXT NOT NULL,
	expires_at   TIMESTAMP NOT NULL,
	created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_refresh ON oauth_tokens(refresh_hash);
`
	if _, err := conn.Exec(schema); err != nil {
		return err
	}

	// Migration: add the role column to DBs created before roles existed, and
	// grandfather those pre-existing tokens as admin (they could already mint).
	if !columnExists(conn, "tokens", "role") {
		if _, err := conn.Exec(`ALTER TABLE tokens ADD COLUMN role TEXT NOT NULL DEFAULT 'member'`); err != nil {
			return err
		}
		if _, err := conn.Exec(`UPDATE tokens SET role = 'admin'`); err != nil {
			return err
		}
	}

	// Migration: add content_hash for ingest de-duplication to DBs created before
	// it existed. The index is created after the column is guaranteed present (it
	// can't live in the schema block above, which runs before this ALTER).
	if !columnExists(conn, "jobs", "content_hash") {
		if _, err := conn.Exec(`ALTER TABLE jobs ADD COLUMN content_hash TEXT`); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_content_hash ON jobs(content_hash)`); err != nil {
		return err
	}

	// Migration: add title for DBs created before it existed. It holds a
	// human-friendly display label — the uploaded filename for an image before
	// it's filed, then the note's own title once filing completes — so the
	// jobs UI never has to render a raw base64 payload or a bare URL.
	if !columnExists(conn, "jobs", "title") {
		if _, err := conn.Exec(`ALTER TABLE jobs ADD COLUMN title TEXT`); err != nil {
			return err
		}
	}
	return nil
}

func columnExists(conn *sql.DB, table, col string) bool {
	rows, err := conn.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == col {
			return true
		}
	}
	return false
}

// genID returns a random 128-bit hex identifier used as a primary key.
func genID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
