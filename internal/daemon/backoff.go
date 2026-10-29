package daemon

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/TomasTomecek/peon/internal/forgejo"
)

const (
	// backoffMax caps the delay between Forgejo attempts while it fails.
	backoffMax = 5 * time.Minute
	// retryAfterMax caps how long a server's Retry-After can pause peon.
	retryAfterMax = 30 * time.Minute
)

// tracked wraps the Forgejo client and feeds every result into the
// backoff state. All calls happen with d.mu held.
type tracked struct {
	inner Forgejo
	d     *Daemon
}

func (t *tracked) ListRepoComments(ctx context.Context, repo string, since time.Time) ([]forgejo.Comment, error) {
	c, err := t.inner.ListRepoComments(ctx, repo, since)
	t.d.forgejoResult(err)
	return c, err
}

func (t *tracked) GetComment(ctx context.Context, repo string, id int64) (forgejo.Comment, error) {
	c, err := t.inner.GetComment(ctx, repo, id)
	t.d.forgejoResult(err)
	return c, err
}

func (t *tracked) ListCommentReactions(ctx context.Context, repo string, id int64) ([]forgejo.Reaction, error) {
	r, err := t.inner.ListCommentReactions(ctx, repo, id)
	t.d.forgejoResult(err)
	return r, err
}

func (t *tracked) CreateComment(ctx context.Context, repo string, issue int64, body string) (forgejo.Comment, error) {
	c, err := t.inner.CreateComment(ctx, repo, issue, body)
	t.d.forgejoResult(err)
	return c, err
}

// forgejoAvailable reports whether peon should talk to Forgejo now, or is
// backing off after failures.
func (d *Daemon) forgejoAvailable() bool {
	return !d.Now().Before(d.fjNext)
}

// forgejoResult updates the backoff state after a Forgejo call. Errors
// that concern one request (a 404 for a deleted card, a 403 on a locked
// issue) say nothing about Forgejo's health and are ignored here.
func (d *Daemon) forgejoResult(err error) {
	if err == nil {
		if d.fjFailures > 0 {
			d.log.Info("Forgejo is reachable again", "failed_attempts", d.fjFailures)
		}
		d.fjFailures, d.fjNext = 0, time.Time{}
		return
	}
	if !forgejo.Transient(err) {
		return
	}
	d.fjFailures++
	delay := d.cfg.Limits.PollInterval.Duration
	for i := 1; i < d.fjFailures && delay < backoffMax; i++ {
		delay *= 2
	}
	if delay > backoffMax {
		delay = backoffMax
	}
	// ±10% jitter so hosts that failed together don't retry together.
	delay += time.Duration(rand.Int64N(int64(delay)/5+1)) - delay/10
	var he *forgejo.HTTPError
	if errors.As(err, &he) && he.RetryAfter > delay {
		delay = min(he.RetryAfter, retryAfterMax)
	}
	d.fjNext = d.Now().Add(delay)
	if d.fjFailures == 1 {
		d.log.Warn("Forgejo is failing; backing off", "retry_in", delay.Round(time.Second), "err", err)
	} else {
		d.log.Debug("Forgejo still failing", "failures", d.fjFailures, "retry_in", delay.Round(time.Second), "err", err)
	}
}
