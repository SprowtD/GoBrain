package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
}

const jobCols = `id, source_kind, payload, COALESCE(note,''), status,
	COALESCE(error,''), COALESCE(output_path,''), COALESCE(token_label,'')`

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

func CreateJob(sourceKind, payload, note, tokenLabel string) (Job, error) {
	id, err := genID()
	if err != nil {
		return Job{}, err
	}
	_, err = db.Exec(
		`INSERT INTO jobs (id, source_kind, payload, note, status, token_label, content_hash)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?)`,
		id, sourceKind, payload, note, tokenLabel, ContentHash(sourceKind, payload),
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
func CreateJobDeduped(sourceKind, payload, note, tokenLabel string) (job Job, duplicate bool, err error) {
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
		`INSERT INTO jobs (id, source_kind, payload, note, status, token_label, content_hash)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?)`,
		id, sourceKind, payload, note, tokenLabel, hash,
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

func ListRecentJobs(limit int) ([]Job, error) {
	rows, err := db.Query(
		`SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC LIMIT ?`, limit,
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

// CompleteJob marks a job filed with the path of its last written chunk.
func CompleteJob(id, outputPath string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = 'filed', output_path = ?, error = NULL,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		outputPath, id,
	)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(s rowScanner) (Job, error) {
	var j Job
	err := s.Scan(&j.ID, &j.SourceKind, &j.Payload, &j.Note,
		&j.Status, &j.Error, &j.OutputPath, &j.TokenLabel)
	return j, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
