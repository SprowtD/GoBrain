package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

// Job is a unit of ingestion work. Timestamps live in the DB (for ordering)
// but are intentionally not surfaced here to keep scanning driver-agnostic.
type Job struct {
	ID         string
	SourceKind string
	Payload    string
	Note       string
	Status     string
	Error      string
	OutputPath string
	TokenLabel string
	// Title is a human-friendly display label: the uploaded filename for an
	// image before it files, then the note's own title once it does. Empty
	// unless set at creation or by CompleteJob at filing time — the UI falls
	// back to a shortened Payload when it's blank.
	Title string
}

const jobCols = `id, source_kind, payload, COALESCE(note,''), status,
	COALESCE(error,''), COALESCE(output_path,''), COALESCE(token_label,''), COALESCE(title,'')`

// jobListCols is jobCols with the payload truncated (SQLite substr counts
// characters) — used by the list path, where payload is display-only.
const jobListCols = `id, source_kind, substr(payload, 1, 200), COALESCE(note,''), status,
	COALESCE(error,''), COALESCE(output_path,''), COALESCE(token_label,''), COALESCE(title,'')`

// dedupeStatuses are the job states that count as "we already have this capture":
// in flight (queued/reading) or successfully filed. A prior 'misfiled' attempt is
// deliberately excluded so a failed capture can be retried.
const dedupeStatuses = `('queued','reading','filed')`

// ContentHash is the de-duplication key for a capture: a hash of its kind and
// payload. The same photo/URL/thought submitted twice yields the same hash, so
// an accidental re-upload can be detected and collapsed onto the first job.
func ContentHash(sourceKind, payload string) string {
	sum := sha256.Sum256([]byte(sourceKind + "\x00" + payload))
	return hex.EncodeToString(sum[:])
}

// CreateJob inserts a job unconditionally (no dedup — the force path). title is
// an optional display label set atomically with the insert, so a poller can
// never observe a title-less row (an uploaded image's payload is a giant
// base64 data: URL that must never render as the row's name).
func CreateJob(sourceKind, payload, note, tokenLabel, title string) (Job, error) {
	id, err := genID()
	if err != nil {
		return Job{}, err
	}
	_, err = db.Exec(
		`INSERT INTO jobs (id, source_kind, payload, note, status, token_label, content_hash, title)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?)`,
		id, sourceKind, payload, note, tokenLabel, ContentHash(sourceKind, payload), nullIfEmpty(title),
	)
	if err != nil {
		return Job{}, err
	}
	return GetJob(id)
}

// CreateJobDeduped inserts a new job unless an identical capture (same content
// hash) is already queued, reading, or filed — in which case it returns that
// existing job with duplicate=true and inserts nothing. The lookup and insert
// run in one transaction so two rapid submits of the same photo (a double tap in
// the app) can't both slip through; SQLite's single writer serializes them.
func CreateJobDeduped(sourceKind, payload, note, tokenLabel, title string) (job Job, duplicate bool, err error) {
	hash := ContentHash(sourceKind, payload)

	tx, err := db.Begin()
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()

	var existingID string
	err = tx.QueryRow(
		`SELECT id FROM jobs WHERE content_hash = ? AND status IN `+dedupeStatuses+`
		 ORDER BY created_at DESC LIMIT 1`, hash,
	).Scan(&existingID)
	switch {
	case err == nil:
		j, gerr := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, existingID))
		if gerr != nil {
			return Job{}, false, gerr
		}
		return j, true, tx.Commit()
	case err != sql.ErrNoRows:
		return Job{}, false, err
	}

	id, err := genID()
	if err != nil {
		return Job{}, false, err
	}
	if _, err = tx.Exec(
		`INSERT INTO jobs (id, source_kind, payload, note, status, token_label, content_hash, title)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?)`,
		id, sourceKind, payload, note, tokenLabel, hash, nullIfEmpty(title),
	); err != nil {
		return Job{}, false, err
	}
	j, err := scanJob(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if err != nil {
		return Job{}, false, err
	}
	return j, false, tx.Commit()
}

func GetJob(id string) (Job, error) {
	row := db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

// ListRecentJobs returns recent jobs newest-first with Payload truncated to a
// display-sized prefix — the list is a summary view (the web fragment polls it
// every few seconds, /v1/status serves it to clients), and an uploaded image's
// payload is a multi-MB base64 data: URL that must not ride along 50 rows at a
// time. GetJob returns the full payload.
func ListRecentJobs(limit int) ([]Job, error) {
	rows, err := db.Query(
		`SELECT `+jobListCols+` FROM jobs ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// UpdateJobStatus sets status and an optional error message.
func UpdateJobStatus(id, status, errMsg string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, nullIfEmpty(errMsg), id,
	)
	return err
}

// CompleteJob marks a job filed with the path of its last written chunk and
// the item's title (overwriting any upload-time title, e.g. a filename) so
// the jobs UI can show a meaningful name instead of a raw payload.
func CompleteJob(id, outputPath, title string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = 'filed', output_path = ?, title = ?, error = NULL,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		outputPath, nullIfEmpty(title), id,
	)
	return err
}

// ErrNotRetryable is returned by RequeueJob when the job isn't in a retryable
// state — it doesn't exist, is already queued/reading, or already filed.
var ErrNotRetryable = errors.New("job is not misfiled")

// RequeueJob flips a misfiled job back to 'queued' for another attempt and
// returns it for enqueueing. Reusing the SAME row (rather than cloning a new
// job) keeps the title and content-hash intact, makes the misfiled row's retry
// button disappear on the next poll, and makes retry idempotent: the status
// guard means a double-click's second UPDATE matches nothing and reports
// ErrNotRetryable instead of filing a duplicate note.
func RequeueJob(id string) (Job, error) {
	res, err := db.Exec(
		`UPDATE jobs SET status = 'queued', error = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = 'misfiled'`, id,
	)
	if err != nil {
		return Job{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Job{}, err
	} else if n == 0 {
		return Job{}, ErrNotRetryable
	}
	return GetJob(id)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(s rowScanner) (Job, error) {
	var j Job
	err := s.Scan(&j.ID, &j.SourceKind, &j.Payload, &j.Note,
		&j.Status, &j.Error, &j.OutputPath, &j.TokenLabel, &j.Title)
	return j, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
