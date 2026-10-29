package job

import (
	"time"

	"github.com/TomasTomecek/peon/internal/forgejo"
)

type Decision int

const (
	Wait Decision = iota
	Approve
	Reject
	Tamper
	Expire
)

func (d Decision) String() string {
	return [...]string{"wait", "approve", "reject", "tamper", "expire"}[d]
}

type Verdict struct {
	Decision Decision
	Reason   string
	Approver forgejo.User
}

// Evaluate applies the approval rules (SPEC.md §6) to a proposed job.
// card is nil when the card comment no longer exists. The order matters:
// integrity first, then an explicit rejection, then expiry, then approval.
func Evaluate(j *Job, card *forgejo.Comment, reactions []forgejo.Reaction, approverID, botID int64, now time.Time) Verdict {
	switch {
	case card == nil:
		return Verdict{Decision: Tamper, Reason: "the card was deleted"}
	case card.ID != j.CardCommentID:
		return Verdict{Decision: Tamper, Reason: "unexpected card comment"}
	case card.User.ID != botID:
		return Verdict{Decision: Tamper, Reason: "the card is not authored by the peon bot"}
	case card.Body != j.CardBody:
		return Verdict{Decision: Tamper, Reason: "the card text was edited"}
	case !card.UpdatedAt.Equal(j.CardUpdatedAt):
		return Verdict{Decision: Tamper, Reason: "the card was edited"}
	}

	var up, down *forgejo.Reaction
	for i := range reactions {
		r := &reactions[i]
		if r.User.ID != approverID {
			continue
		}
		switch r.Content {
		case "+1":
			up = r
		case "-1":
			down = r
		}
	}
	if down != nil {
		return Verdict{Decision: Reject, Approver: down.User}
	}
	if !now.Before(j.Record.ExpiresAt) {
		return Verdict{Decision: Expire}
	}
	if up != nil {
		return Verdict{Decision: Approve, Approver: up.User}
	}
	return Verdict{Decision: Wait}
}

// KillRequested reports whether the approver has put 👎 on a running job.
func KillRequested(reactions []forgejo.Reaction, approverID int64) bool {
	for _, r := range reactions {
		if r.User.ID == approverID && r.Content == "-1" {
			return true
		}
	}
	return false
}
