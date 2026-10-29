package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/TomasTomecek/peon/internal/forgejo"
	"github.com/TomasTomecek/peon/internal/job"
	"github.com/TomasTomecek/peon/internal/state"
)

const (
	// outboxGiveUp is how long peon keeps trying to post a comment.
	outboxGiveUp = 24 * time.Hour
	outboxRetry  = 10 * time.Second
	outboxMaxGap = 10 * time.Minute
)

// post queues a status or result comment for j and tries to deliver the
// queue. The comment is saved before the first attempt, so it survives a
// Forgejo outage and a restart. Agents wait for these comments, so they
// must not be lost.
func (d *Daemon) post(ctx context.Context, j *job.Job, kind, body string) {
	key := j.Record.JobID + ":" + kind
	for _, o := range d.st.Outbox {
		if o.Key == key {
			return
		}
	}
	now := d.Now()
	d.st.Outbox = append(d.st.Outbox, &state.Outgoing{
		Key: key, JobID: j.Record.JobID, Kind: kind,
		Repo: j.Record.Repo, Issue: j.Record.Issue, Body: body,
		QueuedAt: now, NextAttempt: now,
	})
	d.save()
	d.flushOutbox(ctx)
}

func outboxDelay(attempts int) time.Duration {
	delay := outboxRetry
	for i := 1; i < attempts && delay < outboxMaxGap; i++ {
		delay *= 2
	}
	if delay > outboxMaxGap {
		delay = outboxMaxGap
	}
	return delay
}

// flushOutbox posts queued comments in order. It stops at the first one
// that is not due or fails, so comments on an issue never arrive out of
// order. A comment is removed from the queue only after Forgejo accepted
// it; a crash in between can at worst post it twice, never lose it.
func (d *Daemon) flushOutbox(ctx context.Context) {
	for len(d.st.Outbox) > 0 {
		o := d.st.Outbox[0]
		now := d.Now()
		if now.Before(o.NextAttempt) {
			return
		}
		_, err := d.fj.CreateComment(ctx, o.Repo, o.Issue, o.Body)
		if err == nil {
			d.st.Outbox = d.st.Outbox[1:]
			d.save()
			if o.Attempts > 0 {
				d.log.Info("posted delayed comment", "job", o.JobID, "kind", o.Kind, "attempts", o.Attempts+1)
				d.outboxEvent(ctx, o, "comment_posted")
			}
			continue
		}
		o.Attempts++
		o.LastError = err.Error()
		// A permanent error (locked or deleted issue, no permission) would
		// otherwise block every later comment for a day.
		if permanent(err) || now.Sub(o.QueuedAt) >= outboxGiveUp {
			d.st.Outbox = d.st.Outbox[1:]
			d.save()
			d.log.Error("giving up on posting comment; the outcome is only in Loki", "job", o.JobID, "kind", o.Kind, "attempts", o.Attempts, "err", err)
			d.outboxEvent(ctx, o, "comment_dropped")
			continue
		}
		o.NextAttempt = now.Add(outboxDelay(o.Attempts))
		d.save()
		d.log.Warn("posting comment failed, will retry", "job", o.JobID, "kind", o.Kind, "attempts", o.Attempts, "next", o.NextAttempt, "err", err)
		return
	}
}

func (d *Daemon) outboxEvent(ctx context.Context, o *state.Outgoing, name string) {
	j := d.st.Jobs[o.JobID]
	if j == nil {
		return
	}
	d.event(ctx, j, name, map[string]any{
		"kind":       o.Kind,
		"attempts":   o.Attempts,
		"last_error": o.LastError,
	}, false)
}

// permanent reports whether Forgejo refused the comment in a way retrying
// cannot fix: a 4xx other than 408 (timeout) and 429 (rate limited).
func permanent(err error) bool {
	var he *forgejo.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status >= 400 && he.Status < 500 && he.Status != 408 && he.Status != 429
}
