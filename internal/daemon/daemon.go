// Package daemon is peon's main loop (SPEC.md §5-§9): it reads /run
// comments, posts job cards, applies the approval rules, runs approved jobs
// one at a time and reports the results.
package daemon

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TomasTomecek/peon/internal/command"
	"github.com/TomasTomecek/peon/internal/config"
	"github.com/TomasTomecek/peon/internal/executor"
	"github.com/TomasTomecek/peon/internal/forgejo"
	"github.com/TomasTomecek/peon/internal/job"
	"github.com/TomasTomecek/peon/internal/loki"
	"github.com/TomasTomecek/peon/internal/state"
)

// Forgejo is the subset of the Forgejo API the daemon uses.
type Forgejo interface {
	ListRepoComments(ctx context.Context, repo string, since time.Time) ([]forgejo.Comment, error)
	GetComment(ctx context.Context, repo string, id int64) (forgejo.Comment, error)
	ListCommentReactions(ctx context.Context, repo string, id int64) ([]forgejo.Reaction, error)
	CreateComment(ctx context.Context, repo string, issue int64, body string) (forgejo.Comment, error)
}

const (
	// cursorOverlap re-reads a little history each poll so a comment whose
	// timestamp lands just behind the cursor is not missed. Handled
	// comments are deduplicated by ID.
	cursorOverlap = 2 * time.Minute
	// keepHistory is how long finished jobs and handled comments are kept.
	keepHistory = 30 * 24 * time.Hour
)

type Daemon struct {
	cfg   *config.Config
	fj    Forgejo
	audit loki.Pusher
	spool *loki.Spool
	exec  executor.Executor
	st    *state.Store
	log   *slog.Logger

	Now  func() time.Time
	Rand io.Reader

	mu      sync.Mutex // guards st, running and the Forgejo backoff state
	running *runningJob
	wg      sync.WaitGroup

	fjFailures int       // consecutive transient Forgejo failures
	fjNext     time.Time // no Forgejo calls before this
}

type runningJob struct {
	id     string
	cancel context.CancelFunc
}

func New(cfg *config.Config, fj Forgejo, audit loki.Pusher, spool *loki.Spool, ex executor.Executor, st *state.Store, log *slog.Logger) *Daemon {
	d := &Daemon{
		cfg: cfg, audit: audit, spool: spool, exec: ex, st: st, log: log,
		Now:  func() time.Time { return time.Now().UTC() },
		Rand: crand.Reader,
	}
	d.fj = &tracked{inner: fj, d: d}
	return d
}

// Run recovers from a previous crash, then polls until ctx is cancelled.
// A job that is running when ctx is cancelled is stopped and reported as
// interrupted before Run returns.
func (d *Daemon) Run(ctx context.Context) error {
	d.Recover(ctx)
	for {
		d.Tick(ctx)
		interval := d.cfg.Limits.PollInterval.Duration
		jitter := time.Duration(rand.Int64N(int64(interval)/5+1)) - interval/10
		select {
		case <-ctx.Done():
			d.Wait()
			return nil
		case <-time.After(interval + jitter):
		}
	}
}

// Wait blocks until no job is running.
func (d *Daemon) Wait() { d.wg.Wait() }

// Tick runs one poll cycle.
func (d *Daemon) Tick(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.st.Health.LastPoll = d.Now()
	d.flushSpool(ctx)
	// While Forgejo is failing, skip it but keep doing local work at the
	// normal cadence: flushing the audit spool and starting approved jobs.
	if d.forgejoAvailable() {
		d.flushOutbox(ctx)
	}
	for _, repo := range d.cfg.Forgejo.Repos {
		if !d.forgejoAvailable() {
			break
		}
		if err := d.scan(ctx, repo); err != nil && !forgejo.Transient(err) {
			d.log.Warn("scanning comments failed", "repo", repo, "err", err)
		}
	}
	if d.forgejoAvailable() {
		d.reviewProposed(ctx)
	}
	if d.forgejoAvailable() {
		d.watchRunning(ctx)
	}
	d.startNext(ctx)
	d.st.Prune(d.Now(), keepHistory)
	d.save()
}

func (d *Daemon) save() bool {
	if err := d.st.Save(); err != nil {
		d.log.Error("saving state failed", "err", err)
		return false
	}
	return true
}

func (d *Daemon) flushSpool(ctx context.Context) {
	if d.spool == nil || d.spool.Pending() == 0 {
		return
	}
	n, err := d.spool.Flush(ctx, d.audit)
	d.lokiResult(err)
	if err != nil {
		d.log.Warn("Loki still unreachable, audit records stay spooled", "sent", n, "pending", d.spool.Pending(), "err", err)
		return
	}
	d.log.Info("flushed spooled audit records", "batches", n)
}

func (d *Daemon) isRequester(id int64) bool {
	for _, r := range d.cfg.Forgejo.RequesterIDs {
		if r == id {
			return true
		}
	}
	return false
}

// scan reads new comments in repo and turns /run comments for this host
// into jobs.
func (d *Daemon) scan(ctx context.Context, repo string) error {
	now := d.Now()
	cursor, ok := d.st.Cursors[repo]
	if !ok {
		return d.startWatching(ctx, repo)
	}
	comments, err := d.fj.ListRepoComments(ctx, repo, cursor.Add(-cursorOverlap))
	if err != nil {
		return err
	}
	sort.Slice(comments, func(i, k int) bool { return comments[i].ID < comments[k].ID })
	newest := cursor
	for _, c := range comments {
		if c.UpdatedAt.After(newest) {
			newest = c.UpdatedAt
		}
		if c.User.ID == d.cfg.Forgejo.BotUserID {
			continue
		}
		req, perr := command.Parse(c.Body)
		if errors.Is(perr, command.ErrNotRun) || req.Host != d.cfg.Host {
			continue
		}
		if _, done := d.st.Handled[c.ID]; done {
			continue
		}
		if err := d.handleRun(ctx, repo, c, req, perr); err != nil {
			// Not marked handled: it is retried on the next poll.
			d.log.Warn("handling /run comment failed, will retry", "repo", repo, "comment", c.ID, "err", err)
			continue
		}
		d.st.Handled[c.ID] = now
		d.save()
	}
	// The cursor only ever holds Forgejo's own timestamps, so a clock
	// difference between peon and Forgejo cannot make it skip comments.
	d.st.Cursors[repo] = newest
	return nil
}

// startWatching runs on the first start for a repo. Existing /run
// comments are marked handled without acting on them, and the cursor is
// taken from Forgejo's timestamps rather than peon's clock.
func (d *Daemon) startWatching(ctx context.Context, repo string) error {
	now := d.Now()
	since := now.Add(-keepHistory)
	comments, err := d.fj.ListRepoComments(ctx, repo, since)
	if err != nil {
		return err
	}
	cursor, ignored := since, 0
	for _, c := range comments {
		if c.UpdatedAt.After(cursor) {
			cursor = c.UpdatedAt
		}
		if req, perr := command.Parse(c.Body); !errors.Is(perr, command.ErrNotRun) && req.Host == d.cfg.Host {
			d.st.Handled[c.ID] = now
			ignored++
		}
	}
	d.st.Cursors[repo] = cursor
	d.log.Info("watching repo; existing /run comments are not acted on", "repo", repo, "ignored", ignored)
	return nil
}

// handleRun creates a job from a /run comment for this host, or replies
// with why it cannot. A returned error means "try again later".
func (d *Daemon) handleRun(ctx context.Context, repo string, c forgejo.Comment, req command.Request, perr error) error {
	issue, err := c.IssueIndex()
	if err != nil {
		d.log.Warn("ignoring /run comment without an issue", "repo", repo, "comment", c.ID, "err", err)
		return nil
	}
	reply := func(e error) error {
		d.log.Info("rejected /run comment", "repo", repo, "issue", issue, "comment", c.ID, "user", c.User.Login, "err", e)
		_, err := d.fj.CreateComment(ctx, repo, issue, job.RenderParseError(d.cfg.Host, c.ID, c.User.Login, e))
		return err
	}
	if !d.isRequester(c.User.ID) {
		return reply(fmt.Errorf("@%s is not allowed to request jobs on %s", c.User.Login, d.cfg.Host))
	}
	if perr != nil {
		return reply(perr)
	}
	runAs := req.User
	if runAs == "" {
		runAs = d.cfg.Jobs.DefaultUser
	}
	if !d.cfg.UserAllowed(runAs) {
		return reply(fmt.Errorf("jobs on %s may not run as %q (allowed: %s)", d.cfg.Host, runAs, strings.Join(d.cfg.Jobs.AllowedUsers, ", ")))
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = d.cfg.Limits.DefaultTimeout.Duration
	}
	if timeout > d.cfg.Limits.MaxTimeout.Duration {
		return reply(fmt.Errorf("--timeout=%s is over this host's maximum of %s", req.Timeout, d.cfg.Limits.MaxTimeout))
	}
	warnings, verr := command.Validate(req.Command, d.cfg.Limits.MaxCommandBytes)
	if verr != nil {
		return reply(verr)
	}

	id, err := job.NewID(d.Now(), d.Rand)
	now := d.Now().Truncate(time.Second)
	if err != nil {
		return err
	}
	j := &job.Job{
		Record: job.Record{
			JobID: id, Host: d.cfg.Host, Repo: repo, Issue: issue,
			SourceCommentID: c.ID, RequesterID: c.User.ID, Requester: c.User.Login,
			User: runAs, Command: req.Command, Private: req.Private, Timeout: timeout,
			CreatedAt: now, ExpiresAt: now.Add(d.cfg.Limits.JobTTL.Duration),
		},
		Warnings: warnings,
		State:    job.Proposed,
	}
	j.Hash = j.Record.Hash()

	body := job.RenderCard(j)
	card, err := d.fj.CreateComment(ctx, repo, issue, body)
	if err != nil {
		return fmt.Errorf("posting job card: %w", err)
	}
	j.CardCommentID, j.CardBody, j.CardUpdatedAt = card.ID, card.Body, card.UpdatedAt
	d.st.Jobs[id] = j
	if card.Body != body {
		// Forgejo did not store the card we sent, so what the operator
		// would approve is not what we rendered. Void the job.
		d.finishUnrun(ctx, j, job.Tampered, "Forgejo stored a different card text than peon sent", job.RenderTampered)
		return nil
	}
	d.event(ctx, j, "proposed", map[string]any{"command": j.Record.Command, "warnings": warnings}, false)
	d.log.Info("job proposed", "job", id, "repo", repo, "issue", issue, "requester", c.User.Login)
	return nil
}

// reviewProposed applies the approval rules to every proposed job.
func (d *Daemon) reviewProposed(ctx context.Context) {
	for _, j := range d.jobsIn(job.Proposed) {
		if !d.forgejoAvailable() {
			return
		}
		var card *forgejo.Comment
		c, err := d.fj.GetComment(ctx, j.Record.Repo, j.CardCommentID)
		switch {
		case forgejo.IsNotFound(err):
		case err != nil:
			if !forgejo.Transient(err) {
				d.log.Warn("reading job card failed", "job", j.Record.JobID, "err", err)
			}
			continue
		default:
			card = &c
		}
		var reactions []forgejo.Reaction
		if card != nil {
			reactions, err = d.fj.ListCommentReactions(ctx, j.Record.Repo, j.CardCommentID)
			if err != nil {
				if !forgejo.Transient(err) {
					d.log.Warn("reading reactions failed", "job", j.Record.JobID, "err", err)
				}
				continue
			}
		}
		v := job.Evaluate(j, card, reactions, d.cfg.Forgejo.ApproverID, d.cfg.Forgejo.BotUserID, d.Now())
		switch v.Decision {
		case job.Wait:
		case job.Tamper:
			d.finishUnrun(ctx, j, job.Tampered, v.Reason, job.RenderTampered)
		case job.Reject:
			j.Approver = v.Approver.Login
			d.finishUnrun(ctx, j, job.Rejected, "", job.RenderRejected)
		case job.Expire:
			d.finishUnrun(ctx, j, job.Expired, "", job.RenderExpired)
		case job.Approve:
			j.Approver = v.Approver.Login
			// Fail closed: no approval is acted on unless it is in Loki.
			if err := d.event(ctx, j, "approved", nil, true); err != nil {
				j.Approver = ""
				d.log.Warn("approval not recorded in Loki, will retry", "job", j.Record.JobID, "err", err)
				continue
			}
			j.State, j.ApprovedAt = job.Approved, d.Now()
			d.save()
			d.log.Info("job approved", "job", j.Record.JobID, "approver", j.Approver)
		}
	}
}

// finishUnrun ends a job that never ran, records it and tells the issue.
func (d *Daemon) finishUnrun(ctx context.Context, j *job.Job, s job.State, reason string, render func(*job.Job) string) {
	j.State, j.Reason, j.FinishedAt = s, reason, d.Now()
	d.save()
	d.event(ctx, j, string(s), map[string]any{"reason": reason}, false)
	d.post(ctx, j, string(s), render(j))
	d.log.Info("job ended without running", "job", j.Record.JobID, "state", s, "reason", reason)
}

func (d *Daemon) jobsIn(s job.State) []*job.Job {
	var out []*job.Job
	for _, j := range d.st.Jobs {
		if j.State == s {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].ApprovedAt.Equal(out[k].ApprovedAt) {
			return out[i].ApprovedAt.Before(out[k].ApprovedAt)
		}
		// Same poll: the order the jobs were requested in.
		if out[i].Record.SourceCommentID != out[k].Record.SourceCommentID {
			return out[i].Record.SourceCommentID < out[k].Record.SourceCommentID
		}
		return out[i].Record.JobID < out[k].Record.JobID
	})
	return out
}
