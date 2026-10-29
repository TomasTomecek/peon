package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomasTomecek/peon/internal/job"
)

var t0 = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

func TestRoundTripAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Cursors["ops/infra"] = t0
	s.Handled[1234] = t0
	s.Jobs["A"] = &job.Job{Record: job.Record{JobID: "A", Command: "uptime"}, State: job.Running}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second Open error = %v, want lock error", err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !s2.Cursors["ops/infra"].Equal(t0) || !s2.Handled[1234].Equal(t0) || s2.Jobs["A"].State != job.Running {
		t.Fatalf("state not restored: %+v", s2.Data)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file mode %v", fi.Mode())
	}
}

func TestCorruptOrUnknownVersionRefused(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"corrupt": "{", "version": `{"version": 9}`} {
		path := filepath.Join(dir, name+".json")
		os.WriteFile(path, []byte(body), 0o600)
		if s, err := Open(path); err == nil {
			s.Close()
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

func TestPrune(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "state.json"))
	defer s.Close()
	now := t0.Add(60 * 24 * time.Hour)
	s.Jobs["old-done"] = &job.Job{State: job.Succeeded, FinishedAt: t0}
	s.Jobs["old-expired"] = &job.Job{State: job.Expired, Record: job.Record{ExpiresAt: t0}}
	s.Jobs["old-running"] = &job.Job{State: job.Running, StartedAt: t0}
	s.Jobs["new-done"] = &job.Job{State: job.Failed, FinishedAt: now}
	s.Handled[1] = t0
	s.Handled[2] = now
	s.Prune(now, 30*24*time.Hour)
	if _, ok := s.Jobs["old-done"]; ok {
		t.Error("old finished job kept")
	}
	if _, ok := s.Jobs["old-expired"]; ok {
		t.Error("old expired job kept")
	}
	if _, ok := s.Jobs["old-running"]; !ok {
		t.Error("non-terminal job pruned")
	}
	if _, ok := s.Jobs["new-done"]; !ok {
		t.Error("recent job pruned")
	}
	if _, ok := s.Handled[1]; ok || len(s.Handled) != 1 {
		t.Errorf("handled = %v", s.Handled)
	}
}
