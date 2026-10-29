// Package state persists peon's state in a single JSON file, replaced
// atomically on every save. The file is the source of truth for job state:
// a job is marked running on disk before its command starts, so a crash
// can never cause a command to run twice.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/TomasTomecek/peon/internal/job"
)

const version = 1

type Data struct {
	Version int `json:"version"`
	// Cursors holds, per repo, the newest comment update time processed.
	Cursors map[string]time.Time `json:"cursors"`
	// Handled holds /run comments for this host that were turned into a
	// job or answered with an error, so they are never handled twice.
	Handled map[int64]time.Time `json:"handled"`
	Jobs    map[string]*job.Job `json:"jobs"`
	// Outbox holds comments peon still has to post, oldest first.
	Outbox []*Outgoing `json:"outbox,omitempty"`
	// Health records when peon last reached Forgejo and Loki, for
	// "peon status".
	Health Health `json:"health"`
}

// Outgoing is a comment waiting to be posted to Forgejo.
type Outgoing struct {
	Key         string    `json:"key"` // job ID and kind; unique
	JobID       string    `json:"job_id"`
	Kind        string    `json:"kind"`
	Repo        string    `json:"repo"`
	Issue       int64     `json:"issue"`
	Body        string    `json:"body"`
	QueuedAt    time.Time `json:"queued_at"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
}

type Health struct {
	LastPoll     time.Time `json:"last_poll,omitempty"`
	ForgejoOK    time.Time `json:"forgejo_ok,omitempty"`
	ForgejoError string    `json:"forgejo_error,omitempty"`
	// ForgejoRetryAt is set while peon backs off from a failing Forgejo.
	ForgejoRetryAt time.Time `json:"forgejo_retry_at,omitempty"`
	LokiOK         time.Time `json:"loki_ok,omitempty"`
	LokiError      string    `json:"loki_error,omitempty"`
}

// Read loads a state file without locking it, for read-only inspection
// while the daemon runs. Saves are atomic renames, so a reader always sees
// a complete file. A missing file is an empty state.
func Read(path string) (*Data, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Data{Version: version}, nil
	}
	if err != nil {
		return nil, err
	}
	var d Data
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("state %s is corrupt: %w", path, err)
	}
	if d.Version != version {
		return nil, fmt.Errorf("state %s has version %d, this peon understands %d", path, d.Version, version)
	}
	return &d, nil
}

type Store struct {
	path string
	lock *os.File
	Data
}

// Open loads the state file, creating an empty state if it does not exist,
// and takes an exclusive lock so two daemons can never share it. A file
// that exists but cannot be parsed is an error: peon must not start with
// state it does not understand.
func Open(path string) (*Store, error) {
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("state %s is locked by another peon process: %w", path, err)
	}
	s := &Store{path: path, lock: lock}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.Data = Data{Version: version}
	case err != nil:
		lock.Close()
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.Data); err != nil {
			lock.Close()
			return nil, fmt.Errorf("state %s is corrupt: %w", path, err)
		}
		if s.Version != version {
			lock.Close()
			return nil, fmt.Errorf("state %s has version %d, this peon understands %d", path, s.Version, version)
		}
	}
	if s.Cursors == nil {
		s.Cursors = map[string]time.Time{}
	}
	if s.Handled == nil {
		s.Handled = map[int64]time.Time{}
	}
	if s.Jobs == nil {
		s.Jobs = map[string]*job.Job{}
	}
	return s, nil
}

func (s *Store) Close() error { return s.lock.Close() }

// Save writes the state to a temporary file, syncs it, renames it over the
// old file and syncs the directory.
func (s *Store) Save() error {
	b, err := json.MarshalIndent(s.Data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Prune forgets terminal jobs and handled comments older than keep.
func (s *Store) Prune(now time.Time, keep time.Duration) {
	cutoff := now.Add(-keep)
	for id, j := range s.Jobs {
		end := j.FinishedAt
		if end.IsZero() {
			end = j.Record.ExpiresAt
		}
		if j.State.Terminal() && end.Before(cutoff) {
			delete(s.Jobs, id)
		}
	}
	for id, t := range s.Handled {
		if t.Before(cutoff) {
			delete(s.Handled, id)
		}
	}
}
