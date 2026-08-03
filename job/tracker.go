package job

import "sync"

// Job lifecycle states. These strings are what the frontend polls for, and
// they are also what gets persisted in the jobs table.
const (
	StatusQueued    = "queued"
	StatusAnalyzing = "analyzing"
	StatusEncoding  = "encoding"
	StatusDone      = "done"
	StatusError     = "error"
)

// Status is the live view of a job, served by GET /api/status/:id.
type Status struct {
	JobID           string  `json:"job_id"`
	Status          string  `json:"status"`
	OverallProgress float64 `json:"overall_progress"`
	EstimatedSizeMB float64 `json:"estimated_size_mb,omitempty"`
	Encoder         string  `json:"encoder,omitempty"`
	Error           string  `json:"error,omitempty"`
}

// tracker holds progress in memory. ffmpeg reports progress several times a
// second, which is far too chatty for the single sqlite connection; the
// database only records state transitions.
type tracker struct {
	mu sync.RWMutex
	m  map[string]Status
}

func newTracker() *tracker {
	return &tracker{m: make(map[string]Status)}
}

func (t *tracker) set(s Status) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[s.JobID] = s
}

// update mutates an existing entry in place. It is a no-op for unknown jobs.
func (t *tracker) update(id string, fn func(*Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[id]
	if !ok {
		return
	}
	fn(&s)
	t.m[id] = s
}

func (t *tracker) get(id string) (Status, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.m[id]
	return s, ok
}

func (t *tracker) delete(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, id)
}
