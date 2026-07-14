package store

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

func CreateJob(sourceKind, payload, note, tokenLabel string) (Job, error) {
	id, err := genID()
	if err != nil {
		return Job{}, err
	}
	_, err = db.Exec(
		`INSERT INTO jobs (id, source_kind, payload, note, status, token_label)
		 VALUES (?, ?, ?, ?, 'queued', ?)`,
		id, sourceKind, payload, note, tokenLabel,
	)
	if err != nil {
		return Job{}, err
	}
	return GetJob(id)
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
