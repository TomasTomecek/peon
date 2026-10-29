package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TomasTomecek/peon/internal/config"
	"github.com/TomasTomecek/peon/internal/executor"
	"github.com/TomasTomecek/peon/internal/forgejo"
	"github.com/TomasTomecek/peon/internal/loki"
	"github.com/TomasTomecek/peon/internal/state"
)

const (
	repo       = "ops/infra"
	issue      = int64(42)
	approverID = int64(1)
	botID      = int64(31)
	agentID    = int64(7)
	strangerID = int64(99)
)

var users = map[int64]string{approverID: "tomas", botID: "peon", agentID: "agent-ops", strangerID: "rando"}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeForgejo is an in-memory Forgejo with one issue.
type fakeForgejo struct {
	mu        sync.Mutex
	clock     *clock
	nextID    int64
	comments  map[int64]*forgejo.Comment
	reactions map[int64][]forgejo.Reaction
	postErr   error
	readErr   error // returned by every read while set
	calls     int   // Forgejo API calls made
	// skew is how far Forgejo's clock is behind peon's.
	skew time.Duration
}

func newFakeForgejo(c *clock) *fakeForgejo {
	return &fakeForgejo{clock: c, nextID: 1000, comments: map[int64]*forgejo.Comment{}, reactions: map[int64][]forgejo.Reaction{}}
}

func (f *fakeForgejo) add(userID int64, body string) *forgejo.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	now := f.clock.Now().Add(-f.skew)
	c := &forgejo.Comment{
		ID: f.nextID, Body: body, User: forgejo.User{ID: userID, Login: users[userID]},
		IssueURL:  fmt.Sprintf("https://forgejo.internal/%s/issues/%d", repo, issue),
		CreatedAt: now, UpdatedAt: now,
	}
	f.comments[c.ID] = c
	return c
}

func (f *fakeForgejo) edit(id int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[id].Body = body
	f.comments[id].UpdatedAt = f.clock.Now().Add(time.Second - f.skew)
}

func (f *fakeForgejo) remove(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.comments, id)
}

func (f *fakeForgejo) react(id, userID int64, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions[id] = append(f.reactions[id], forgejo.Reaction{User: forgejo.User{ID: userID, Login: users[userID]}, Content: content})
}

// botComments returns the bodies of comments posted by peon, in order.
func (f *fakeForgejo) botComments() []*forgejo.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*forgejo.Comment
	for _, c := range f.comments {
		if c.User.ID == botID {
			cc := *c
			out = append(out, &cc)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

func (f *fakeForgejo) setReadErr(err error) { f.mu.Lock(); f.readErr = err; f.mu.Unlock() }

func (f *fakeForgejo) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (f *fakeForgejo) ListRepoComments(_ context.Context, r string, since time.Time) ([]forgejo.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.readErr != nil {
		return nil, f.readErr
	}
	var out []forgejo.Comment
	for _, c := range f.comments {
		if r == repo && !c.UpdatedAt.Before(since) {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *fakeForgejo) GetComment(_ context.Context, _ string, id int64) (forgejo.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.readErr != nil {
		return forgejo.Comment{}, f.readErr
	}
	c, ok := f.comments[id]
	if !ok {
		return forgejo.Comment{}, &forgejo.HTTPError{Status: http.StatusNotFound}
	}
	return *c, nil
}

func (f *fakeForgejo) ListCommentReactions(_ context.Context, _ string, id int64) ([]forgejo.Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.readErr != nil {
		return nil, f.readErr
	}
	return append([]forgejo.Reaction(nil), f.reactions[id]...), nil
}

func (f *fakeForgejo) setPostErr(err error) { f.mu.Lock(); f.postErr = err; f.mu.Unlock() }

func (f *fakeForgejo) CreateComment(_ context.Context, _ string, iss int64, body string) (forgejo.Comment, error) {
	f.mu.Lock()
	postErr := f.postErr
	f.mu.Unlock()
	if postErr != nil {
		return forgejo.Comment{}, postErr
	}
	if iss != issue {
		return forgejo.Comment{}, fmt.Errorf("unexpected issue %d", iss)
	}
	return *f.add(botID, body), nil
}

type fakeLoki struct {
	mu      sync.Mutex
	fail    bool
	entries []loki.Entry
}

func (l *fakeLoki) Push(_ context.Context, e []loki.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return errors.New("loki down")
	}
	l.entries = append(l.entries, e...)
	return nil
}

func (l *fakeLoki) setFail(v bool) { l.mu.Lock(); l.fail = v; l.mu.Unlock() }

// events returns "event" labels of pushed entries, in order.
func (l *fakeLoki) events(jobID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if e.Labels["job_id"] == jobID {
			out = append(out, e.Labels["event"])
		}
	}
	return out
}

func (l *fakeLoki) lines(jobID, event string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if e.Labels["job_id"] == jobID && e.Labels["event"] == event {
			out = append(out, e.Line)
		}
	}
	return out
}

// fakeExec records what it is asked to run. A job blocks until release is
// closed (if set) or ctx is cancelled.
type fakeExec struct {
	mu       sync.Mutex
	ran      []executor.Spec
	stopped  []string
	stdout   []string
	exit     int
	release  chan struct{}
	onRun    func(executor.Spec)
	started  chan string
	timedOut bool
}

func (x *fakeExec) Run(ctx context.Context, s executor.Spec, stdout, stderr func(string)) executor.Result {
	x.mu.Lock()
	x.ran = append(x.ran, s)
	onRun, release, started := x.onRun, x.release, x.started
	x.mu.Unlock()
	if onRun != nil {
		onRun(s)
	}
	if started != nil {
		started <- s.JobID
	}
	for _, l := range x.stdout {
		stdout(l)
	}
	stderr("from stderr")
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return executor.Result{ExitCode: 143}
		}
	}
	return executor.Result{ExitCode: x.exit, TimedOut: x.timedOut}
}

func (x *fakeExec) Stop(_ context.Context, id string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.stopped = append(x.stopped, id)
	return nil
}

func (x *fakeExec) runs() []executor.Spec {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]executor.Spec(nil), x.ran...)
}

type harness struct {
	t     *testing.T
	clock *clock
	fj    *fakeForgejo
	loki  *fakeLoki
	exec  *fakeExec
	st    *state.Store
	d     *Daemon
	dir   string
}

func testConfig() *config.Config {
	c := &config.Config{Host: "db1"}
	c.Forgejo.Repos = []string{repo}
	c.Forgejo.BotUserID = botID
	c.Forgejo.ApproverID = approverID
	c.Forgejo.RequesterIDs = []int64{agentID, approverID}
	c.Jobs.DefaultUser = "root"
	c.Jobs.AllowedUsers = []string{"root", "deploy"}
	c.Limits.JobTTL.Duration = 30 * time.Minute
	c.Limits.DefaultTimeout.Duration = 10 * time.Minute
	c.Limits.MaxTimeout.Duration = time.Hour
	c.Limits.MaxCommandBytes = 4096
	c.Limits.OutputTailLines = 50
	c.Limits.PollInterval.Duration = 10 * time.Second
	c.Grafana.ExploreURL = "https://grafana.internal/explore"
	return c
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clock: &clock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
	h.fj = newFakeForgejo(h.clock)
	h.loki = &fakeLoki{}
	h.exec = &fakeExec{stdout: []string{"hello", "world"}}
	h.open()
	// First tick: start watching the repo from now on.
	h.tick()
	h.clock.Add(time.Second)
	return h
}

func (h *harness) open() {
	st, err := state.Open(filepath.Join(h.dir, "state.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	spool, err := loki.NewSpool(filepath.Join(h.dir, "spool"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.st = st
	h.d = New(testConfig(), h.fj, h.loki, spool, h.exec, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.d.Now = h.clock.Now
	h.t.Cleanup(func() { h.d.Wait(); st.Close() })
}

// restart simulates peon being restarted with the same state directory.
func (h *harness) restart() {
	h.d.Wait()
	h.st.Close()
	h.open()
}

func (h *harness) tick() {
	h.d.Tick(context.Background())
}

// tickAndWait ticks and waits for any job started by it to finish.
func (h *harness) tickAndWait() {
	h.tick()
	h.d.Wait()
}

func (h *harness) propose(body string) (cardID int64, jobID string) {
	h.t.Helper()
	h.fj.add(agentID, body)
	before := len(h.fj.botComments())
	h.tick()
	bc := h.fj.botComments()
	if len(bc) != before+1 {
		h.t.Fatalf("expected a job card, bot comments: %d -> %d", before, len(bc))
	}
	card := bc[len(bc)-1]
	if !strings.Contains(card.Body, "peon job") {
		h.t.Fatalf("not a job card:\n%s", card.Body)
	}
	for id, j := range h.st.Jobs {
		if j.CardCommentID == card.ID {
			return card.ID, id
		}
	}
	h.t.Fatal("no job for card")
	return 0, ""
}

func (h *harness) lastBot() string {
	bc := h.fj.botComments()
	if len(bc) == 0 {
		return ""
	}
	return bc[len(bc)-1].Body
}
