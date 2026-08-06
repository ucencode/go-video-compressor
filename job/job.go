package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Job is one unit of work: compress Path using the named preset.
type Job struct {
	ID       string
	FileID   string
	Path     string // uploaded source file on disk
	OrigName string // filename as the user uploaded it
	Preset   string
}

var ErrQueueFull = errors.New("compression queue is full, try again shortly")

// Queue accepts jobs and runs them on a fixed pool of workers.
type Queue struct {
	ch      chan Job
	db      *sql.DB
	tracker *tracker
	enc     Encoder
	outDir  string
}

// NewQueue starts the worker pool. Keep workers low: encoding is the bottleneck
// and running several at once on one GPU only makes each of them slower.
func NewQueue(db *sql.DB, outDir string, buffer, workers int) *Queue {
	q := &Queue{
		ch:      make(chan Job, buffer),
		db:      db,
		tracker: newTracker(),
		enc:     DetectEncoder(),
		outDir:  outDir,
	}
	for range workers {
		go q.run()
	}
	return q
}

// Encoder reports which encoder the queue will use.
func (q *Queue) Encoder() Encoder { return q.enc }

// Submit enqueues a job without blocking. It returns ErrQueueFull if the buffer
// is saturated, so uploads fail fast instead of hanging the request.
func (q *Queue) Submit(j Job) error {
	// Register before sending, otherwise a worker can pick the job up and set
	// "analyzing" before this goroutine writes "queued" over it.
	q.tracker.set(Status{
		JobID:   j.ID,
		Status:  StatusQueued,
		Encoder: q.enc.Display,
	})

	select {
	case q.ch <- j:
		return nil
	default:
		q.tracker.delete(j.ID)
		return ErrQueueFull
	}
}

// Status returns the live status of a job, falling back to the database for
// jobs that finished before the process restarted.
func (q *Queue) Status(id string) (Status, bool) {
	if s, ok := q.tracker.get(id); ok {
		return s, true
	}

	var status string
	var errMsg sql.NullString
	err := q.db.QueryRow(`SELECT status, error FROM jobs WHERE id = ?`, id).
		Scan(&status, &errMsg)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("status lookup for job %s: %v", id, err)
		}
		return Status{}, false
	}

	s := Status{JobID: id, Status: status, Error: errMsg.String}
	if status == StatusDone {
		s.OverallProgress = 100
	}
	return s, true
}

// Output describes the compressed file of a finished job.
type Output struct {
	Path     string
	Filename string
}

// ErrNoOutput means the job exists but has not produced a file.
var ErrNoOutput = errors.New("job has no compressed file")

// Output returns the compressed file of a completed job. It returns
// sql.ErrNoRows for an unknown job and ErrNoOutput when nothing was produced.
func (q *Queue) Output(id string) (Output, error) {
	var out Output
	err := q.db.QueryRow(
		`SELECT COALESCE(f.path, ''), COALESCE(f.filename, '')
		 FROM jobs j LEFT JOIN files f ON f.id = j.output_file_id
		 WHERE j.id = ?`, id,
	).Scan(&out.Path, &out.Filename)
	if err != nil {
		return Output{}, err
	}
	if out.Path == "" {
		return Output{}, ErrNoOutput
	}
	return out, nil
}

// Summary is one row of the job list served to the UI.
type Summary struct {
	JobID           string    `json:"job_id"`
	Filename        string    `json:"filename"`
	Status          string    `json:"status"`
	OverallProgress float64   `json:"overall_progress"`
	EstimatedSizeMB float64   `json:"estimated_size_mb,omitempty"`
	Error           string    `json:"error,omitempty"`
	SourceSize      int64     `json:"source_size"`
	OutputSize      int64     `json:"output_size"`
	Preset          string    `json:"preset,omitempty"`
	Encoder         string    `json:"encoder,omitempty"`
	CanRetry        bool      `json:"can_retry"`
	CreatedAt       time.Time `json:"created_at"`
}

// List returns every job, newest first, with live progress merged in for the
// ones still running.
func (q *Queue) List() ([]Summary, error) {
	rows, err := q.db.Query(
		`SELECT j.id, j.status, COALESCE(j.error, ''), COALESCE(j.preset, ''),
		        COALESCE(j.encoder, ''), j.created_at,
		        COALESCE(src.filename, ''), COALESCE(src.size, 0), COALESCE(src.path, ''),
		        COALESCE(out.size, 0)
		 FROM jobs j
		 LEFT JOIN files src ON src.id = j.file_id
		 LEFT JOIN files out ON out.id = j.output_file_id
		 ORDER BY j.created_at DESC, j.id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summaries := []Summary{}
	for rows.Next() {
		var (
			s       Summary
			srcPath string
		)
		if err := rows.Scan(
			&s.JobID, &s.Status, &s.Error, &s.Preset, &s.Encoder, &s.CreatedAt,
			&s.Filename, &s.SourceSize, &srcPath, &s.OutputSize,
		); err != nil {
			return nil, err
		}

		// Only a failed job with its source still on disk can be re-run.
		if s.Status == StatusError && srcPath != "" {
			if _, err := os.Stat(srcPath); err == nil {
				s.CanRetry = true
			}
		}
		if s.Status == StatusDone {
			s.OverallProgress = 100
		}
		// The database only sees state transitions; percentages live in memory.
		if live, ok := q.tracker.get(s.JobID); ok {
			s.Status = live.Status
			s.OverallProgress = live.OverallProgress
			s.EstimatedSizeMB = live.EstimatedSizeMB
			s.Error = live.Error
			if live.Encoder != "" {
				s.Encoder = live.Encoder
			}
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// ErrJobActive is returned when an operation would disturb a running job.
var ErrJobActive = errors.New("job is still running")

// ErrSourceGone means the upload is no longer on disk, so there is nothing to
// re-run.
var ErrSourceGone = errors.New("original upload is no longer available")

// Retry re-runs a failed job under its original ID, so it keeps its place in
// the list rather than appearing as a new entry.
func (q *Queue) Retry(id string) error {
	var (
		status  string
		preset  string
		fileID  string
		srcPath string
		name    string
	)
	err := q.db.QueryRow(
		`SELECT j.status, COALESCE(j.preset, ''), j.file_id,
		        COALESCE(f.path, ''), COALESCE(f.filename, '')
		 FROM jobs j LEFT JOIN files f ON f.id = j.file_id
		 WHERE j.id = ?`, id,
	).Scan(&status, &preset, &fileID, &srcPath, &name)
	if err != nil {
		return err
	}
	if isActive(status) {
		return ErrJobActive
	}
	if srcPath == "" {
		return ErrSourceGone
	}
	if _, err := os.Stat(srcPath); err != nil {
		return ErrSourceGone
	}

	if _, err := q.db.Exec(
		`UPDATE jobs SET status = ?, error = '', output_file_id = NULL,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		StatusQueued, id,
	); err != nil {
		return err
	}

	return q.Submit(Job{
		ID:       id,
		FileID:   fileID,
		Path:     srcPath,
		OrigName: name,
		Preset:   preset,
	})
}

// Delete removes a job and every file it owns.
func (q *Queue) Delete(id string) error {
	var (
		status  string
		fileID  string
		outID   sql.NullString
		srcPath string
		outPath string
	)
	err := q.db.QueryRow(
		`SELECT j.status, j.file_id, j.output_file_id,
		        COALESCE(src.path, ''), COALESCE(out.path, '')
		 FROM jobs j
		 LEFT JOIN files src ON src.id = j.file_id
		 LEFT JOIN files out ON out.id = j.output_file_id
		 WHERE j.id = ?`, id,
	).Scan(&status, &fileID, &outID, &srcPath, &outPath)
	if err != nil {
		return err
	}
	if isActive(status) {
		return ErrJobActive
	}

	// Rows go first: if an unlink fails the job is already gone from the UI,
	// which beats leaving a row that points at a half-deleted set of files.
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM jobs WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE id = ?`, fileID); err != nil {
		return err
	}
	if outID.Valid {
		if _, err := tx.Exec(`DELETE FROM files WHERE id = ?`, outID.String); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	for _, path := range []string{srcPath, outPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("could not remove %s for job %s: %v", path, id, err)
		}
	}
	q.tracker.delete(id)
	return nil
}

func isActive(status string) bool {
	switch status {
	case StatusQueued, StatusAnalyzing, StatusEncoding:
		return true
	}
	return false
}

func (q *Queue) run() {
	for j := range q.ch {
		q.process(j)
	}
}

func (q *Queue) process(j Job) {
	preset, ok := LookupPreset(j.Preset)
	if !ok {
		q.fail(j.ID, fmt.Errorf("unknown preset %q", j.Preset))
		return
	}

	q.setStatus(j.ID, StatusAnalyzing)
	info, err := Probe(j.Path)
	if err != nil {
		q.fail(j.ID, err)
		return
	}

	out := filepath.Join(q.outDir, j.ID+".mp4")
	q.setStatus(j.ID, StatusEncoding)

	req := EncodeRequest{
		Input:  j.Path,
		Output: out,
		Preset: preset,
		Source: info,
	}
	err = Encode(context.Background(), q.enc, req, func(frac float64) {
		q.tracker.update(j.ID, func(s *Status) {
			s.OverallProgress = frac * 100
			// Extrapolate the final size from how much has been written so far.
			// Below a few percent the estimate swings wildly, so hold it back.
			if frac > 0.05 {
				if fi, err := os.Stat(out); err == nil {
					s.EstimatedSizeMB = toMB(float64(fi.Size()) / frac)
				}
			}
		})
	})
	if err != nil {
		os.Remove(out) // don't leave a truncated file behind
		q.fail(j.ID, err)
		return
	}

	var outSize int64
	if fi, err := os.Stat(out); err == nil {
		outSize = fi.Size()
	}

	q.tracker.update(j.ID, func(s *Status) {
		s.Status = StatusDone
		s.OverallProgress = 100
		s.EstimatedSizeMB = toMB(float64(outSize))
	})

	if err := q.recordOutput(j, out, outSize); err != nil {
		q.fail(j.ID, fmt.Errorf("recording output: %w", err))
		return
	}

	// The source has served its purpose: the output exists and retry is only
	// offered for failures. Dropping it keeps uploads/ from growing forever.
	if err := os.Remove(j.Path); err != nil && !os.IsNotExist(err) {
		log.Printf("could not remove source of job %s: %v", j.ID, err)
	} else if _, err := q.db.Exec(`UPDATE files SET path = '' WHERE id = ?`, j.FileID); err != nil {
		log.Printf("could not clear source path of job %s: %v", j.ID, err)
	}

	log.Printf("job %s done: %s (%d bytes)", j.ID, out, outSize)
}

// recordOutput registers the compressed file and links it to the job. Both
// writes happen together so a job can never point at a file with no row.
func (q *Queue) recordOutput(j Job, path string, size int64) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	outputFileID := NewID()
	if _, err := tx.Exec(
		`INSERT INTO files (id, filename, size, path, kind) VALUES (?, ?, ?, ?, 'compressed')`,
		outputFileID, OutputName(j.OrigName), size, path,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, error = '', output_file_id = ?, encoder = ?,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		StatusDone, outputFileID, q.enc.Name, j.ID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *Queue) setStatus(id, status string) {
	q.tracker.update(id, func(s *Status) { s.Status = status })
	if _, err := q.db.Exec(
		`UPDATE jobs SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, id,
	); err != nil {
		log.Printf("failed to update job %s: %v", id, err)
	}
}

func (q *Queue) fail(id string, cause error) {
	msg := cause.Error()
	log.Printf("job %s failed: %s", id, msg)

	q.tracker.update(id, func(s *Status) {
		s.Status = StatusError
		s.Error = msg
	})
	if _, err := q.db.Exec(
		`UPDATE jobs SET status = ?, error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		StatusError, msg, id,
	); err != nil {
		log.Printf("failed to record failure of job %s: %v", id, err)
	}
}

func toMB(bytes float64) float64 {
	mb := bytes / (1024 * 1024)
	return float64(int(mb*10+0.5)) / 10 // one decimal place
}
