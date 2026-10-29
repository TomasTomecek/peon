// Package job holds the job model (SPEC.md §5-§7): the immutable record
// that approval is bound to, its hash, the job state machine, the comments
// peon posts, and the approval rules.
package job

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type State string

const (
	Proposed    State = "proposed"
	Approved    State = "approved"
	Running     State = "running"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	TimedOut    State = "timed_out"
	Killed      State = "killed"
	Interrupted State = "interrupted"
	Rejected    State = "rejected"
	Expired     State = "expired"
	Tampered    State = "tampered"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	switch s {
	case Proposed, Approved, Running:
		return false
	}
	return true
}

// Record is everything the approval covers. It never changes after the
// job card is posted; the command peon executes is Record.Command.
type Record struct {
	JobID           string        `json:"job_id"`
	Host            string        `json:"host"`
	Repo            string        `json:"repo"`
	Issue           int64         `json:"issue"`
	SourceCommentID int64         `json:"source_comment_id"`
	RequesterID     int64         `json:"requester_id"`
	Requester       string        `json:"requester"`
	Command         string        `json:"command"`
	Private         bool          `json:"private"`
	Timeout         time.Duration `json:"timeout"`
	CreatedAt       time.Time     `json:"created_at"`
	ExpiresAt       time.Time     `json:"expires_at"`
}

// Canonical returns the canonical JSON of the record: an object with
// sorted keys, no insignificant whitespace, UTF-8, times as RFC 3339 UTC
// with second precision and the timeout as a Go duration string.
func (r Record) Canonical() []byte {
	m := map[string]any{
		"job_id":            r.JobID,
		"host":              r.Host,
		"repo":              r.Repo,
		"issue":             r.Issue,
		"source_comment_id": r.SourceCommentID,
		"requester_id":      r.RequesterID,
		"requester":         r.Requester,
		"command":           r.Command,
		"flags": map[string]any{
			"private": r.Private,
			"timeout": r.Timeout.String(),
		},
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at": r.ExpiresAt.UTC().Format(time.RFC3339),
	}
	// encoding/json sorts map keys, so the output is deterministic.
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // only plain strings, numbers and bools above
	}
	return b
}

// Hash is the hex SHA-256 of the canonical record.
func (r Record) Hash() string {
	sum := sha256.Sum256(r.Canonical())
	return hex.EncodeToString(sum[:])
}

// Job is a record plus everything peon learns about it over its life.
type Job struct {
	Record   Record   `json:"record"`
	Hash     string   `json:"hash"`
	Warnings []string `json:"warnings,omitempty"`
	State    State    `json:"state"`
	Reason   string   `json:"reason,omitempty"`

	// The job card exactly as Forgejo returned it after posting.
	CardCommentID int64     `json:"card_comment_id"`
	CardBody      string    `json:"card_body"`
	CardUpdatedAt time.Time `json:"card_updated_at"`

	Approver    string    `json:"approver,omitempty"`
	ApprovedAt  time.Time `json:"approved_at,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	KillPending bool      `json:"kill_pending,omitempty"`
}
