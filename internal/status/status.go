// Package status builds the read-only report behind "peon status".
package status

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TomasTomecek/peon/internal/job"
	"github.com/TomasTomecek/peon/internal/state"
)

// recentJobs is how many finished jobs the report lists.
const recentJobs = 10

type Job struct {
	ID         string    `json:"id"`
	State      job.State `json:"state"`
	Repo       string    `json:"repo"`
	Issue      int64     `json:"issue"`
	Requester  string    `json:"requester"`
	Approver   string    `json:"approver,omitempty"`
	User       string    `json:"user"`
	Command    string    `json:"command"`
	Reason     string    `json:"reason,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	ApprovedAt time.Time `json:"approved_at,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type Spool struct {
	Batches int   `json:"batches"`
	Bytes   int64 `json:"bytes"`
	Corrupt int   `json:"corrupt"`
}

// Outbox summarizes comments waiting to be posted to Forgejo.
type Outbox struct {
	Pending   int       `json:"pending"`
	Oldest    time.Time `json:"oldest,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

type Report struct {
	Host    string               `json:"host,omitempty"`
	Now     time.Time            `json:"now"`
	Running *Job                 `json:"running,omitempty"`
	Queued  []Job                `json:"queued"`
	Waiting []Job                `json:"waiting"`
	Recent  []Job                `json:"recent"`
	Cursors map[string]time.Time `json:"cursors"`
	Health  state.Health         `json:"health"`
	Spool   Spool                `json:"spool"`
	Outbox  Outbox               `json:"outbox"`
}

func summarize(j *job.Job) Job {
	r := j.Record
	user := r.User
	if user == "" {
		user = "root"
	}
	return Job{
		ID: r.JobID, State: j.State, Repo: r.Repo, Issue: r.Issue,
		Requester: r.Requester, Approver: j.Approver, User: user,
		Command: r.Command, Reason: j.Reason, ExitCode: j.ExitCode,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ApprovedAt: j.ApprovedAt,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

// Collect reads the state file and spool in stateDir. It never writes.
func Collect(stateDir, host string, now time.Time) (*Report, error) {
	d, err := state.Read(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return nil, err
	}
	r := &Report{Host: host, Now: now, Cursors: d.Cursors, Health: d.Health,
		Queued: []Job{}, Waiting: []Job{}, Recent: []Job{}}
	if r.Cursors == nil {
		r.Cursors = map[string]time.Time{}
	}
	var recent []Job
	for _, j := range d.Jobs {
		s := summarize(j)
		switch j.State {
		case job.Running:
			r.Running = &s
		case job.Approved:
			r.Queued = append(r.Queued, s)
		case job.Proposed:
			r.Waiting = append(r.Waiting, s)
		default:
			recent = append(recent, s)
		}
	}
	sort.Slice(r.Queued, func(i, k int) bool { return r.Queued[i].ApprovedAt.Before(r.Queued[k].ApprovedAt) })
	sort.Slice(r.Waiting, func(i, k int) bool { return r.Waiting[i].CreatedAt.Before(r.Waiting[k].CreatedAt) })
	sort.Slice(recent, func(i, k int) bool { return end(recent[i]).After(end(recent[k])) })
	if len(recent) > recentJobs {
		recent = recent[:recentJobs]
	}
	if recent != nil {
		r.Recent = recent
	}
	for _, o := range d.Outbox {
		r.Outbox.Pending++
		if r.Outbox.Oldest.IsZero() || o.QueuedAt.Before(r.Outbox.Oldest) {
			r.Outbox.Oldest = o.QueuedAt
		}
		if o.LastError != "" {
			r.Outbox.LastError = o.LastError
		}
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, "spool"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".corrupt"):
			r.Spool.Corrupt++
		case strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, "."):
			r.Spool.Batches++
			if fi, err := e.Info(); err == nil {
				r.Spool.Bytes += fi.Size()
			}
		}
	}
	return r, nil
}

func end(j Job) time.Time {
	if !j.FinishedAt.IsZero() {
		return j.FinishedAt
	}
	return j.ExpiresAt
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		return "in " + round(-d)
	}
	return round(d) + " ago"
}

func round(d time.Duration) string {
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// firstLine shortens a command to one readable line.
func firstLine(cmd string) string {
	line, rest, multi := strings.Cut(cmd, "\n")
	if len(line) > 60 {
		line = line[:57] + "..."
	} else if multi && rest != "" {
		line += " …"
	}
	return line
}

func bytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Text renders the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	title := "peon"
	if r.Host != "" {
		title += " on " + r.Host
	}
	fmt.Fprintf(&b, "%s, %s\n\n", title, r.Now.UTC().Format("2006-01-02 15:04:05 UTC"))

	b.WriteString("Running\n")
	if r.Running == nil {
		b.WriteString("  nothing\n")
	} else {
		j := r.Running
		fmt.Fprintf(&b, "  %s  %s\n      as %s, asked by %s, approved by %s, running for %s\n",
			j.ID, firstLine(j.Command), j.User, j.Requester, j.Approver, round(r.Now.Sub(j.StartedAt)))
	}

	fmt.Fprintf(&b, "\nQueued (approved, waiting to start): %d\n", len(r.Queued))
	for _, j := range r.Queued {
		fmt.Fprintf(&b, "  %s  %s\n      as %s, asked by %s, approved by %s, must start by %s\n",
			j.ID, firstLine(j.Command), j.User, j.Requester, j.Approver, ago(r.Now, j.ExpiresAt))
	}

	fmt.Fprintf(&b, "\nWaiting for approval: %d\n", len(r.Waiting))
	for _, j := range r.Waiting {
		fmt.Fprintf(&b, "  %s  %s\n      as %s, asked by %s in %s#%d, expires %s\n",
			j.ID, firstLine(j.Command), j.User, j.Requester, j.Repo, j.Issue, ago(r.Now, j.ExpiresAt))
	}

	b.WriteString("\nRecent\n")
	if len(r.Recent) == 0 {
		b.WriteString("  none\n")
	}
	for _, j := range r.Recent {
		outcome := string(j.State)
		if j.ExitCode != nil {
			outcome += fmt.Sprintf(" (exit %d)", *j.ExitCode)
		}
		fmt.Fprintf(&b, "  %s  %-22s %s, %s\n", j.ID, outcome, firstLine(j.Command), ago(r.Now, end(j)))
	}

	h := r.Health
	b.WriteString("\nHealth\n")
	fmt.Fprintf(&b, "  last poll  %s\n", ago(r.Now, h.LastPoll))
	fmt.Fprintf(&b, "  forgejo    last ok %s", ago(r.Now, h.ForgejoOK))
	if h.ForgejoError != "" {
		fmt.Fprintf(&b, ", failing: %s", h.ForgejoError)
	}
	if h.ForgejoRetryAt.After(r.Now) {
		fmt.Fprintf(&b, ", backing off, next try %s", ago(r.Now, h.ForgejoRetryAt))
	}
	fmt.Fprintf(&b, "\n  loki       last ok %s", ago(r.Now, h.LokiOK))
	if h.LokiError != "" {
		fmt.Fprintf(&b, ", failing: %s", h.LokiError)
	}
	fmt.Fprintf(&b, "\n  spool      %d batch(es), %s", r.Spool.Batches, bytes(r.Spool.Bytes))
	if r.Spool.Corrupt > 0 {
		fmt.Fprintf(&b, ", %d corrupt file(s) kept for inspection", r.Spool.Corrupt)
	}
	fmt.Fprintf(&b, "\n  outbox     %d comment(s) waiting to post", r.Outbox.Pending)
	if r.Outbox.Pending > 0 {
		fmt.Fprintf(&b, ", oldest queued %s", ago(r.Now, r.Outbox.Oldest))
		if r.Outbox.LastError != "" {
			fmt.Fprintf(&b, ", last error: %s", r.Outbox.LastError)
		}
	}
	b.WriteString("\n")

	if len(r.Cursors) > 0 {
		b.WriteString("\nRepos (newest comment seen)\n")
		repos := make([]string, 0, len(r.Cursors))
		for repo := range r.Cursors {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			fmt.Fprintf(&b, "  %-30s %s\n", repo, ago(r.Now, r.Cursors[repo]))
		}
	}
	return b.String()
}
