package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomasTomecek/peon/internal/job"
	"github.com/TomasTomecek/peon/internal/state"
)

var now = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := func(id, cmd string, created time.Time) job.Record {
		return job.Record{JobID: id, Host: "db1", Repo: "ops/infra", Issue: 42, Requester: "agent-ops",
			User: "root", Command: cmd, Timeout: 10 * time.Minute, CreatedAt: created, ExpiresAt: created.Add(30 * time.Minute)}
	}
	zero, one := 0, 1
	st.Jobs["RUN"] = &job.Job{Record: rec("RUN", "dnf -y upgrade", now.Add(-5*time.Minute)), State: job.Running,
		Approver: "tomas", ApprovedAt: now.Add(-4 * time.Minute), StartedAt: now.Add(-3 * time.Minute)}
	st.Jobs["Q1"] = &job.Job{Record: rec("Q1", "systemctl restart nginx", now.Add(-2*time.Minute)), State: job.Approved,
		Approver: "tomas", ApprovedAt: now.Add(-time.Minute)}
	st.Jobs["W1"] = &job.Job{Record: rec("W1", "cd /srv\nmake install", now.Add(-time.Minute)), State: job.Proposed}
	for i, s := range []job.State{job.Succeeded, job.Failed, job.Rejected} {
		id := []string{"D1", "D2", "D3"}[i]
		j := &job.Job{Record: rec(id, "uptime", now.Add(-time.Hour)), State: s, FinishedAt: now.Add(-time.Duration(i+1) * time.Minute)}
		if s == job.Succeeded {
			j.ExitCode = &zero
		}
		if s == job.Failed {
			j.ExitCode = &one
		}
		st.Jobs[id] = j
	}
	for i := 0; i < 15; i++ {
		id := "OLD" + string(rune('A'+i))
		st.Jobs[id] = &job.Job{Record: rec(id, "true", now.Add(-48*time.Hour)), State: job.Succeeded, FinishedAt: now.Add(-24 * time.Hour)}
	}
	st.Cursors["ops/infra"] = now.Add(-20 * time.Second)
	st.Outbox = []*state.Outgoing{
		{Key: "D2:result", JobID: "D2", QueuedAt: now.Add(-3 * time.Minute), LastError: "forgejo: HTTP 502"},
		{Key: "D3:rejected", JobID: "D3", QueuedAt: now.Add(-time.Minute)},
	}
	st.Health = state.Health{LastPoll: now.Add(-5 * time.Second), ForgejoOK: now.Add(-5 * time.Second),
		ForgejoError: "forgejo: HTTP 502", ForgejoRetryAt: now.Add(40 * time.Second),
		LokiOK: now.Add(-10 * time.Minute), LokiError: "loki: HTTP 502: bad gateway"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(dir, "spool")
	os.MkdirAll(spool, 0o700)
	os.WriteFile(filepath.Join(spool, "00000000000000000001-000001.json"), make([]byte, 2048), 0o600)
	os.WriteFile(filepath.Join(spool, "00000000000000000002-000002.json"), make([]byte, 1024), 0o600)
	os.WriteFile(filepath.Join(spool, ".00000000000000000003-000003.json"), make([]byte, 99), 0o600) // in flight
	os.WriteFile(filepath.Join(spool, "00000000000000000000-000000.json.corrupt"), nil, 0o600)
	return dir
}

func TestCollect(t *testing.T) {
	r, err := Collect(fixture(t), "db1", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Running == nil || r.Running.ID != "RUN" || len(r.Queued) != 1 || len(r.Waiting) != 1 {
		t.Fatalf("running %+v, queued %d, waiting %d", r.Running, len(r.Queued), len(r.Waiting))
	}
	if len(r.Recent) != recentJobs || r.Recent[0].ID != "D1" || r.Recent[1].ID != "D2" || r.Recent[2].ID != "D3" {
		t.Fatalf("recent %+v", r.Recent)
	}
	if r.Spool != (Spool{Batches: 2, Bytes: 3072, Corrupt: 1}) {
		t.Fatalf("spool %+v", r.Spool)
	}
	if r.Outbox.Pending != 2 || !r.Outbox.Oldest.Equal(now.Add(-3*time.Minute)) {
		t.Fatalf("outbox %+v", r.Outbox)
	}
}

func TestText(t *testing.T) {
	r, _ := Collect(fixture(t), "db1", now)
	out := r.Text()
	for _, want := range []string{
		"peon on db1, 2026-10-29 10:00:00 UTC",
		"RUN  dnf -y upgrade", "as root, asked by agent-ops, approved by tomas, running for 3m0s",
		"Queued (approved, waiting to start): 1", "must start by in 28m0s",
		"Waiting for approval: 1", "W1  cd /srv …", "in ops/infra#42, expires in 29m0s",
		"D1  succeeded (exit 0)", "D2  failed (exit 1)", "D3  rejected",
		"last poll  5s ago", "loki       last ok 10m0s ago, failing: loki: HTTP 502: bad gateway",
		"spool      2 batch(es), 3.0 KiB, 1 corrupt file(s)",
		"forgejo    last ok 5s ago, failing: forgejo: HTTP 502, backing off, next try in 40s",
		"outbox     2 comment(s) waiting to post, oldest queued 3m0s ago, last error: forgejo: HTTP 502",
		"ops/infra", "20s ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestJSONAndEmptyState(t *testing.T) {
	r, err := Collect(t.TempDir(), "", now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"queued":[]`) || !strings.Contains(string(b), `"recent":[]`) || strings.Contains(string(b), `"running"`) {
		t.Fatalf("json %s", b)
	}
	if !strings.Contains(r.Text(), "Running\n  nothing") || !strings.Contains(r.Text(), "last poll  never") {
		t.Fatalf("text:\n%s", r.Text())
	}
}

func TestCollectWhileDaemonHoldsTheLock(t *testing.T) {
	dir := fixture(t)
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := Collect(dir, "db1", now); err != nil {
		t.Fatalf("status blocked by the daemon's lock: %v", err)
	}
}
