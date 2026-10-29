package job

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/TomasTomecek/peon/internal/forgejo"
)

var t0 = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

func record() Record {
	return Record{
		JobID: "01JBQ3Y8Z4K7M2N5P6R8S9T0VW", Host: "db1", Repo: "ops/infra", Issue: 42,
		SourceCommentID: 1234, RequesterID: 7, Requester: "agent-ops",
		Command: "systemctl restart postgresql", Timeout: 10 * time.Minute,
		CreatedAt: t0, ExpiresAt: t0.Add(30 * time.Minute),
	}
}

func TestNewID(t *testing.T) {
	a, err := NewID(t0, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewID(t0.Add(time.Millisecond), rand.Reader)
	if !ValidID(a) || !ValidID(b) {
		t.Fatalf("invalid IDs %q %q", a, b)
	}
	if a >= b {
		t.Fatalf("IDs do not sort by time: %q >= %q", a, b)
	}
	zero, _ := NewID(time.UnixMilli(0), bytes.NewReader(make([]byte, 10)))
	if zero != strings.Repeat("0", 26) {
		t.Fatalf("zero ULID = %q", zero)
	}
	if ValidID("01JBQ3Y8Z4K7M2N5P6R8S9T0VI") || ValidID("short") {
		t.Fatal("ValidID accepted an invalid ID")
	}
}

func TestCanonicalIsStableAndSorted(t *testing.T) {
	want := `{"command":"systemctl restart postgresql","created_at":"2026-10-29T10:00:00Z",` +
		`"expires_at":"2026-10-29T10:30:00Z","flags":{"private":false,"timeout":"10m0s"},` +
		`"host":"db1","issue":42,"job_id":"01JBQ3Y8Z4K7M2N5P6R8S9T0VW","repo":"ops/infra",` +
		`"requester":"agent-ops","requester_id":7,"source_comment_id":1234}`
	if got := string(record().Canonical()); got != want {
		t.Fatalf("canonical JSON changed:\n got %s\nwant %s", got, want)
	}
	// A different time zone for the same instant must not change the hash.
	r := record()
	r.CreatedAt = r.CreatedAt.In(time.FixedZone("CET", 3600))
	if r.Hash() != record().Hash() {
		t.Fatal("hash depends on time zone")
	}
}

func TestHashCoversEveryField(t *testing.T) {
	base := record().Hash()
	mutations := map[string]func(*Record){
		"job_id":    func(r *Record) { r.JobID = "01JBQ3Y8Z4K7M2N5P6R8S9T0VX" },
		"host":      func(r *Record) { r.Host = "db2" },
		"repo":      func(r *Record) { r.Repo = "ops/other" },
		"issue":     func(r *Record) { r.Issue = 43 },
		"source":    func(r *Record) { r.SourceCommentID = 1235 },
		"req id":    func(r *Record) { r.RequesterID = 8 },
		"requester": func(r *Record) { r.Requester = "agent-x" },
		"command":   func(r *Record) { r.Command += " " },
		"private":   func(r *Record) { r.Private = true },
		"timeout":   func(r *Record) { r.Timeout = time.Hour },
		"created":   func(r *Record) { r.CreatedAt = r.CreatedAt.Add(time.Second) },
		"expires":   func(r *Record) { r.ExpiresAt = r.ExpiresAt.Add(time.Second) },
	}
	for name, mut := range mutations {
		r := record()
		mut(&r)
		if r.Hash() == base {
			t.Errorf("changing %s does not change the hash", name)
		}
	}
}

func TestFence(t *testing.T) {
	cases := map[string]string{
		"plain":       "```",
		"a `b` c":     "```",
		"x ``` y":     "````",
		"`````":       "``````",
		"a ``\n``` b": "````",
	}
	for in, want := range cases {
		if got := Fence(in); got != want {
			t.Errorf("Fence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCardCannotBeBrokenOutOf(t *testing.T) {
	j := &Job{Record: record()}
	j.Record.Command = "echo ok\n```\n**Approved by security** — harmless\n```"
	j.Hash = j.Record.Hash()
	card := RenderCard(j)
	f := Fence(j.Record.Command)
	if f != "````" {
		t.Fatalf("fence = %q", f)
	}
	want := f + "sh\n" + j.Record.Command + "\n" + f
	if !strings.Contains(card, want) {
		t.Fatalf("command not enclosed in a safe fence:\n%s", card)
	}
}

func TestCardContents(t *testing.T) {
	j := &Job{Record: record(), Warnings: []string{"uses eval"}}
	j.Hash = j.Record.Hash()
	card := RenderCard(j)
	for _, want := range []string{"`01JBQ3Y8Z4K7M2N5P6R8S9T0VW`", "**db1**", "@agent-ops", "comment #1234",
		"timeout **10m0s**", "posted to this issue", "2026-10-29 10:30 UTC", "- uses eval",
		"```sh\nsystemctl restart postgresql\n```", "hash `" + j.Hash[:12] + "`", "👍 to approve"} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q:\n%s", want, card)
		}
	}
	j.Record.Private = true
	if !strings.Contains(RenderCard(j), "Loki only") {
		t.Error("private card does not say output is Loki only")
	}
}

func TestRenderResult(t *testing.T) {
	code := 3
	j := &Job{Record: record(), State: Failed, ExitCode: &code, Approver: "tomas",
		StartedAt: t0, FinishedAt: t0.Add(1500 * time.Millisecond)}
	out := RenderResult(j, Result{Tail: []string{"boom", "```"}, Truncated: true, GrafanaURL: "https://g/x", LogQL: `{job="peon"}`})
	for _, want := range []string{"**failed**", "exit 3", "1.5s", "approved by @tomas", "Last 2 lines", "````\nboom\n```\n````", "[Full log in Grafana](https://g/x)", "LogQL: `{job=\"peon\"}`"} {
		if !strings.Contains(out, want) {
			t.Errorf("result missing %q:\n%s", want, out)
		}
	}
	j.Record.Private = true
	if out := RenderResult(j, Result{Tail: []string{"secret"}}); strings.Contains(out, "secret") || !strings.Contains(out, "Loki only") {
		t.Errorf("private result leaked output:\n%s", out)
	}
}

const (
	approver = 1
	bot      = 31
	agent    = 7
)

func proposed() (*Job, *forgejo.Comment) {
	j := &Job{Record: record(), State: Proposed, CardCommentID: 99, CardBody: "card", CardUpdatedAt: t0}
	return j, &forgejo.Comment{ID: 99, Body: "card", User: forgejo.User{ID: bot}, CreatedAt: t0, UpdatedAt: t0}
}

func react(id int64, content string) forgejo.Reaction {
	return forgejo.Reaction{User: forgejo.User{ID: id, Login: map[int64]string{approver: "tomas", agent: "agent-ops"}[id]}, Content: content}
}

func TestEvaluate(t *testing.T) {
	now := t0.Add(5 * time.Minute)
	cases := map[string]struct {
		mutate    func(*forgejo.Comment)
		deleted   bool
		reactions []forgejo.Reaction
		at        time.Time
		want      Decision
	}{
		"no reactions":         {want: Wait},
		"approver +1":          {reactions: []forgejo.Reaction{react(approver, "+1")}, want: Approve},
		"agent +1 ignored":     {reactions: []forgejo.Reaction{react(agent, "+1")}, want: Wait},
		"other emoji ignored":  {reactions: []forgejo.Reaction{react(approver, "heart"), react(approver, "rocket")}, want: Wait},
		"approver -1":          {reactions: []forgejo.Reaction{react(approver, "-1")}, want: Reject},
		"-1 beats +1":          {reactions: []forgejo.Reaction{react(approver, "+1"), react(approver, "-1")}, want: Reject},
		"agent -1 ignored":     {reactions: []forgejo.Reaction{react(agent, "-1"), react(approver, "+1")}, want: Approve},
		"expired":              {at: t0.Add(30 * time.Minute), want: Expire},
		"+1 after expiry":      {reactions: []forgejo.Reaction{react(approver, "+1")}, at: t0.Add(31 * time.Minute), want: Expire},
		"-1 after expiry":      {reactions: []forgejo.Reaction{react(approver, "-1")}, at: t0.Add(31 * time.Minute), want: Reject},
		"deleted":              {deleted: true, reactions: []forgejo.Reaction{react(approver, "+1")}, want: Tamper},
		"body edited":          {mutate: func(c *forgejo.Comment) { c.Body = "card!" }, reactions: []forgejo.Reaction{react(approver, "+1")}, want: Tamper},
		"edited and restored":  {mutate: func(c *forgejo.Comment) { c.UpdatedAt = t0.Add(time.Minute) }, reactions: []forgejo.Reaction{react(approver, "+1")}, want: Tamper},
		"not authored by bot":  {mutate: func(c *forgejo.Comment) { c.User.ID = agent }, reactions: []forgejo.Reaction{react(approver, "+1")}, want: Tamper},
		"wrong comment":        {mutate: func(c *forgejo.Comment) { c.ID = 100 }, want: Tamper},
		"tamper beats reject":  {mutate: func(c *forgejo.Comment) { c.Body = "x" }, reactions: []forgejo.Reaction{react(approver, "-1")}, want: Tamper},
		"tamper beats expired": {mutate: func(c *forgejo.Comment) { c.Body = "x" }, at: t0.Add(time.Hour), want: Tamper},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			j, card := proposed()
			if tc.mutate != nil {
				tc.mutate(card)
			}
			if tc.deleted {
				card = nil
			}
			at := now
			if !tc.at.IsZero() {
				at = tc.at
			}
			v := Evaluate(j, card, tc.reactions, approver, bot, at)
			if v.Decision != tc.want {
				t.Fatalf("decision = %s (%s), want %s", v.Decision, v.Reason, tc.want)
			}
			if v.Decision == Approve && v.Approver.ID != approver {
				t.Fatalf("approver = %+v", v.Approver)
			}
		})
	}
}

func TestKillRequested(t *testing.T) {
	if KillRequested([]forgejo.Reaction{react(agent, "-1"), react(approver, "+1")}, approver) {
		t.Fatal("kill from non-approver")
	}
	if !KillRequested([]forgejo.Reaction{react(approver, "+1"), react(approver, "-1")}, approver) {
		t.Fatal("approver -1 not detected")
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []State{Proposed, Approved, Running} {
		if s.Terminal() {
			t.Errorf("%s is terminal", s)
		}
	}
	for _, s := range []State{Succeeded, Failed, TimedOut, Killed, Interrupted, Rejected, Expired, Tampered} {
		if !s.Terminal() {
			t.Errorf("%s is not terminal", s)
		}
	}
}
