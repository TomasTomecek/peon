package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/TomasTomecek/peon/internal/executor"
	"github.com/TomasTomecek/peon/internal/job"
	"github.com/TomasTomecek/peon/internal/loki"
)

func (d *Daemon) labels(j *job.Job, event string) map[string]string {
	l := map[string]string{
		"job":       "peon",
		"host":      j.Record.Host,
		"job_id":    j.Record.JobID,
		"repo":      j.Record.Repo,
		"issue":     strconv.FormatInt(j.Record.Issue, 10),
		"requester": j.Record.Requester,
		"event":     event,
	}
	if j.Approver != "" {
		l["approver"] = j.Approver
	}
	return l
}

// event records a lifecycle event in Loki. With sync, the push must
// succeed and its error is returned (fail closed). Without it, a failed
// push is spooled and retried later.
func (d *Daemon) event(ctx context.Context, j *job.Job, name string, extra map[string]any, sync bool) error {
	rec := map[string]any{
		"event":             name,
		"job_id":            j.Record.JobID,
		"job_hash":          j.Hash,
		"state":             j.State,
		"host":              j.Record.Host,
		"repo":              j.Record.Repo,
		"issue":             j.Record.Issue,
		"source_comment_id": j.Record.SourceCommentID,
		"card_comment_id":   j.CardCommentID,
		"requester":         j.Record.Requester,
		"requester_id":      j.Record.RequesterID,
		"user":              j.Record.User,
	}
	if j.Approver != "" {
		rec["approver"] = j.Approver
	}
	for k, v := range extra {
		if v != nil && v != "" {
			rec[k] = v
		}
	}
	line, _ := json.Marshal(rec)
	entries := []loki.Entry{{Labels: d.labels(j, name), Time: d.Now(), Line: string(line)}}
	err := d.audit.Push(ctx, entries)
	if err == nil || sync {
		return err
	}
	if d.spool != nil {
		if serr := d.spool.Add(entries); serr != nil {
			d.log.Error("audit record lost: Loki and spool both failed", "job", j.Record.JobID, "event", name, "err", err, "spool_err", serr)
			return serr
		}
	}
	return nil
}

// watchRunning stops the running job if the approver puts 👎 on its card.
func (d *Daemon) watchRunning(ctx context.Context) {
	if d.running == nil {
		return
	}
	j := d.st.Jobs[d.running.id]
	if j == nil || j.KillPending {
		return
	}
	reactions, err := d.fj.ListCommentReactions(ctx, j.Record.Repo, j.CardCommentID)
	if err != nil {
		d.log.Warn("reading reactions on running job failed", "job", j.Record.JobID, "err", err)
		return
	}
	if !job.KillRequested(reactions, d.cfg.Forgejo.ApproverID) {
		return
	}
	j.KillPending = true
	d.save()
	d.event(ctx, j, "kill_requested", nil, false)
	d.log.Info("approver asked to kill the running job", "job", j.Record.JobID)
	d.running.cancel()
}

// startNext starts the oldest approved job if nothing is running.
func (d *Daemon) startNext(ctx context.Context) {
	if d.running != nil {
		return
	}
	queue := d.jobsIn(job.Approved)
	if len(queue) == 0 {
		return
	}
	j := queue[0]
	// An approval is for running the job soon, not whenever the queue
	// clears: a job that has not started by its expiry never starts.
	if !d.Now().Before(j.Record.ExpiresAt) {
		d.finishUnrun(ctx, j, job.Expired, "It was approved but could not start before it expired.", job.RenderExpired)
		return
	}
	// Fail closed: nothing runs unless Loki has the start record.
	if err := d.event(ctx, j, "started", map[string]any{"command": j.Record.Command}, true); err != nil {
		d.log.Warn("start not recorded in Loki, will retry", "job", j.Record.JobID, "err", err)
		return
	}
	j.State, j.StartedAt = job.Running, d.Now()
	// The running state must be on disk before the command starts, so a
	// crash can never make it run twice.
	if !d.save() {
		j.State, j.StartedAt = job.Approved, time.Time{}
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	d.running = &runningJob{id: j.Record.JobID, cancel: cancel}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		d.runJob(runCtx, j)
	}()
	d.log.Info("job started", "job", j.Record.JobID)
}

// tail keeps the last lines of output within a line and byte budget.
type tail struct {
	mu        sync.Mutex
	max       int
	lines     []string
	truncated bool
}

const tailBytes = 4 << 10

func (t *tail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
		t.truncated = true
	}
}

func (t *tail) get() ([]string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	size, start := 0, len(t.lines)
	for start > 0 && size+len(t.lines[start-1])+1 <= tailBytes {
		start--
		size += len(t.lines[start]) + 1
	}
	if start == len(t.lines) && start > 0 {
		// A single huge last line: show its end.
		l := t.lines[start-1]
		return []string{"…" + l[len(l)-tailBytes+1:]}, true
	}
	return append([]string(nil), t.lines[start:]...), t.truncated || start > 0
}

func (d *Daemon) runJob(ctx context.Context, j *job.Job) {
	out := loki.NewStream(d.audit, d.spool, d.labels(j, "stdout"))
	errs := loki.NewStream(d.audit, d.spool, d.labels(j, "stderr"))
	tl := &tail{max: d.cfg.Limits.OutputTailLines}

	flushCtx, stopFlush := context.WithCancel(context.Background())
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-tick.C:
				_ = out.Flush(flushCtx)
				_ = errs.Flush(flushCtx)
			}
		}
	}()

	res := d.exec.Run(ctx, executor.Spec{JobID: j.Record.JobID, Command: j.Record.Command, Timeout: j.Record.Timeout, User: j.Record.User},
		func(l string) { out.Add(d.Now(), l); tl.add(l) },
		func(l string) { errs.Add(d.Now(), l); tl.add(l) })

	stopFlush()
	<-flushed
	final, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := out.Flush(final); err != nil {
		d.log.Error("job output lost: Loki and spool both failed", "job", j.Record.JobID, "err", err)
	}
	if err := errs.Flush(final); err != nil {
		d.log.Error("job output lost: Loki and spool both failed", "job", j.Record.JobID, "err", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.running = nil
	j.FinishedAt = d.Now()
	if res.ExitCode >= 0 {
		code := res.ExitCode
		j.ExitCode = &code
	}
	switch {
	case j.KillPending:
		j.State, j.Reason = job.Killed, "Stopped by 👎 from @"+j.Approver+"."
	case ctx.Err() != nil:
		j.State, j.Reason = job.Interrupted, "peon was stopped while the job was running, so the job was stopped too."
	case res.Err != nil:
		j.State, j.Reason = job.Failed, "peon could not run the job: "+res.Err.Error()
	case res.TimedOut:
		j.State, j.Reason = job.TimedOut, fmt.Sprintf("Stopped after the %s timeout.", j.Record.Timeout)
	case res.ExitCode == 0:
		j.State = job.Succeeded
	default:
		j.State = job.Failed
	}
	d.save()
	d.event(final, j, "finished", map[string]any{
		"exit_code":   j.ExitCode,
		"reason":      j.Reason,
		"duration_ms": j.FinishedAt.Sub(j.StartedAt).Milliseconds(),
	}, false)

	lines, truncated := tl.get()
	body := job.RenderResult(j, job.Result{Tail: lines, Truncated: truncated, GrafanaURL: d.grafanaURL(j), LogQL: logQL(j)})
	d.post(final, j, "result", body)
	d.log.Info("job finished", "job", j.Record.JobID, "state", j.State, "exit", res.ExitCode)
}

func logQL(j *job.Job) string {
	return fmt.Sprintf(`{job="peon", job_id="%s"}`, j.Record.JobID)
}

// grafanaURL links to Explore with the job's logs. Without a datasource
// UID it links to Explore itself; the comment carries the LogQL query.
func (d *Daemon) grafanaURL(j *job.Job) string {
	base := d.cfg.Grafana.ExploreURL
	uid := d.cfg.Grafana.LokiDatasourceUID
	if uid == "" {
		return base
	}
	from := j.StartedAt.Add(-time.Minute)
	to := j.FinishedAt.Add(time.Minute)
	if j.StartedAt.IsZero() {
		from = j.Record.CreatedAt.Add(-time.Minute)
	}
	panes := map[string]any{"peon": map[string]any{
		"datasource": uid,
		"queries": []map[string]any{{
			"refId":      "A",
			"expr":       logQL(j),
			"datasource": map[string]string{"type": "loki", "uid": uid},
		}},
		"range": map[string]string{
			"from": strconv.FormatInt(from.UnixMilli(), 10),
			"to":   strconv.FormatInt(to.UnixMilli(), 10),
		},
	}}
	b, _ := json.Marshal(panes)
	return base + "?schemaVersion=1&panes=" + url.QueryEscape(string(b))
}

// Recover handles jobs left running by a previous peon process: the unit
// is stopped and the job is reported as interrupted. It is never re-run.
func (d *Daemon) Recover(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobsIn(job.Running) {
		stopCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := d.exec.Stop(stopCtx, j.Record.JobID)
		cancel()
		reason := "peon restarted while this job was running. Its unit was stopped and any output after the restart was not captured. It will not be run again."
		if err != nil {
			reason = "peon restarted while this job was running and could not stop its unit (" + err.Error() + "). Check the host. It will not be run again."
		}
		j.State, j.Reason, j.FinishedAt = job.Interrupted, reason, d.Now()
		d.save()
		d.event(ctx, j, "finished", map[string]any{"reason": reason}, false)
		body := job.RenderResult(j, job.Result{GrafanaURL: d.grafanaURL(j), LogQL: logQL(j)})
		d.post(ctx, j, "result", body)
		d.log.Warn("recovered interrupted job", "job", j.Record.JobID)
	}
}
