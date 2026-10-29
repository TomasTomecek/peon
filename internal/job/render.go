package job

import (
	"fmt"
	"strings"
	"time"
)

// Fence returns a backtick fence longer than any backtick run in s, so s
// cannot close the code block early and render text outside it. This keeps
// "what you see on the card is what runs" true for commands containing ```.
func Fence(s string) string {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

func fenced(lang, s string) string {
	f := Fence(s)
	return f + lang + "\n" + s + "\n" + f
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// RenderCard renders the job card the operator approves. Its exact bytes
// are stored and compared on every check (SPEC.md §6).
func RenderCard(j *Job) string {
	r := j.Record
	output := "posted to this issue"
	if r.Private {
		output = "**Loki only** (`--private`)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🪓 **peon job** `%s` on **%s**\n\n", r.JobID, r.Host)
	fmt.Fprintf(&b, "Requested by @%s in comment #%d\n", r.Requester, r.SourceCommentID)
	runsAs := "Runs as **" + r.User + "**"
	if r.User != "root" {
		runsAs += " (in their systemd user manager)"
	}
	fmt.Fprintf(&b, "%s · timeout **%s** · output %s\n", runsAs, r.Timeout, output)
	fmt.Fprintf(&b, "Expires **%s**\n\n", r.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	if len(j.Warnings) > 0 {
		b.WriteString("⚠️ **Read carefully:**\n")
		for _, w := range j.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
		b.WriteString("\n")
	}
	b.WriteString(fenced("sh", r.Command))
	fmt.Fprintf(&b, "\n\nhash `%s`\n\n", shortHash(j.Hash))
	b.WriteString("👍 to approve · 👎 to reject")
	return b.String()
}

func header(emoji, verb string, r Record) string {
	return fmt.Sprintf("%s peon job `%s` on **%s** %s", emoji, r.JobID, r.Host, verb)
}

// RenderParseError is the reply to a malformed /run for this host.
func RenderParseError(host string, sourceCommentID int64, requester string, err error) string {
	return fmt.Sprintf("❌ peon on **%s** can't accept comment #%d from @%s:\n\n%s\n\nNo job was created. Post a corrected `/run`.",
		host, sourceCommentID, requester, fenced("", err.Error()))
}

func RenderRejected(j *Job) string {
	return header("🛑", "was **rejected**", j.Record) + ". Nothing ran."
}

func RenderExpired(j *Job) string {
	if j.Reason != "" {
		return header("⌛", "**expired**", j.Record) + ". " + j.Reason + " Nothing ran."
	}
	return header("⌛", "**expired**", j.Record) + " without approval. Nothing ran."
}

func RenderTampered(j *Job) string {
	return header("🚨", "was **not run**", j.Record) +
		fmt.Sprintf(": its card changed after it was posted (%s). The job is void; propose it again if it is still needed.", j.Reason)
}

// Result describes how a job ended, for the result comment.
type Result struct {
	Tail       []string // last lines of combined output
	Truncated  bool     // more output exists than Tail holds
	GrafanaURL string
	LogQL      string
}

func RenderResult(j *Job, res Result) string {
	r := j.Record
	var emoji, verb string
	switch j.State {
	case Succeeded:
		emoji, verb = "✅", "**succeeded**"
	case Failed:
		emoji, verb = "❌", "**failed**"
	case TimedOut:
		emoji, verb = "⏱️", "**timed out**"
	case Killed:
		emoji, verb = "🛑", "was **killed**"
	case Interrupted:
		emoji, verb = "⚠️", "was **interrupted**"
	default:
		emoji, verb = "❔", "ended as **"+string(j.State)+"**"
	}
	var b strings.Builder
	b.WriteString(header(emoji, verb, r))
	var details []string
	if j.ExitCode != nil {
		details = append(details, fmt.Sprintf("exit %d", *j.ExitCode))
	}
	if !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		details = append(details, duration(j.FinishedAt.Sub(j.StartedAt)))
	}
	if j.Approver != "" {
		details = append(details, "approved by @"+j.Approver)
	}
	if len(details) > 0 {
		b.WriteString(" (" + strings.Join(details, ", ") + ")")
	}
	b.WriteString("\n")
	if j.Reason != "" {
		fmt.Fprintf(&b, "\n%s\n", j.Reason)
	}
	switch {
	case r.Private:
		b.WriteString("\nOutput is in Loki only (`--private`).\n")
	case len(res.Tail) == 0:
		b.WriteString("\nNo output.\n")
	default:
		if res.Truncated {
			fmt.Fprintf(&b, "\nLast %d lines:\n", len(res.Tail))
		}
		b.WriteString("\n" + fenced("", strings.Join(res.Tail, "\n")) + "\n")
	}
	b.WriteString("\n")
	if res.GrafanaURL != "" {
		fmt.Fprintf(&b, "[Full log in Grafana](%s) · ", res.GrafanaURL)
	}
	fmt.Fprintf(&b, "LogQL: `%s`", res.LogQL)
	return b.String()
}
