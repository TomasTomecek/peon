# Using peon

## The flow

1. An agent posts a `/run` comment in an issue.
2. The named host replies with a **job card**.
3. You read the card and react 👍 to run it, or 👎 to reject it.
4. peon runs it and posts the **result**.

Everything is also recorded in Loki, which is the authoritative record of
what ran.

## For operators

### Reading a job card

````
🪓 peon job `01JBQ3Y8Z4K7M2N5P6R8S9T0VW` on **db1**

Requested by @agent-ops in comment #1234
Runs as **root** · timeout **10m0s** · output posted to this issue
Expires **2026-10-29 10:30 UTC**

⚠️ **Read carefully:**
- pipes a download into a shell

```sh
curl -fsSL https://example.com/install.sh | sh
```

hash `3f9ac21e0b7d`

👍 to approve · 👎 to reject
````

- **What's in the code block is exactly what runs.** peon executes its own
  stored copy, never anything re-read from Forgejo. Commands can only
  contain printable ASCII, so there is no hidden Unicode, and the block
  can't be broken out of.
- **Warnings** flag things that are hard to review on a phone (multiple
  lines, long lines) or often dangerous (`base64`, `curl | sh`, `eval`,
  `rm -rf /`, `dd`, `mkfs`, raw block devices). They never block a job.
  You decide.
- **Runs as** says who the command runs as. Non-root jobs say "(in their
  systemd user manager)".
- **Output** says whether the result will be posted to the issue, or kept
  in Loki only (`--private`).

### Approving, rejecting, killing

| You do | On | Result |
|---|---|---|
| 👍 | a waiting card | The job runs (one at a time per host, in approval order) |
| 👎 | a waiting card | Rejected; nothing runs |
| 👎 | a running job's card | The job is stopped and reported **killed** |
| nothing | a waiting card | After `job_ttl` (default 30 min) the job **expires**; nothing runs |

Only reactions from your account count, matched by user ID. Other
reactions on the card are ignored, so agents can't approve their own
jobs.

An approval is for running the job **soon**. If a job hasn't started by
its expiry, for example because a long job was running ahead of it, it
expires instead of firing late.

### If the card says "not run"

🚨 means the card was edited or deleted after peon posted it. peon then
voids the job, even if the edit was reverted. Nothing ran. If the command
is still needed, the agent has to propose it again.

### Results

| | State | Meaning |
|---|---|---|
| ✅ | succeeded | Exit code 0 |
| ❌ | failed | Non-zero exit code, or peon couldn't start it (the reason is shown) |
| ⏱️ | timed out | Stopped at its timeout |
| 🛑 | killed | Stopped by your 👎 |
| ⚠️ | interrupted | peon stopped or restarted while it ran; the job was stopped and is never re-run |

The result shows the exit code, duration, who approved it, and the last
lines of output (nothing for `--private` jobs). It also links to Grafana
and gives the LogQL query for the full log.

### Finding things in Loki

Every record has the labels `job="peon"`, `host`, `job_id`, `repo`,
`issue`, `requester`, `event`, and `approver` once a job is approved.

```
# everything about one job
{job="peon", job_id="01JBQ3Y8Z4K7M2N5P6R8S9T0VW"}

# every command that started on db1, with who asked and who approved
{job="peon", host="db1", event="started"} | json | line_format "{{.requester}} → {{.approver}}: {{.command}}"

# rejected, expired and voided jobs
{job="peon", event=~"rejected|expired|tampered"}

# a job's output
{job="peon", job_id="01JB…", event=~"stdout|stderr"}
```

Events: `proposed`, `approved`, `rejected`, `expired`, `tampered`,
`started`, `kill_requested`, `stdout`, `stderr`, `finished`. Lifecycle
events are JSON. `stdout`/`stderr` lines are the raw output.

## For agents

### Requesting a command

````
/run db1 --timeout=20m
```sh
dnf -y upgrade openssl
```
````

- The first line is `/run <host>` plus optional flags, followed by
  **exactly one** fenced code block and nothing else in the comment.
- Flags:
  - `--user=<name>`: run as this user, in their systemd user manager.
    Must be allowed on that host.
  - `--private`: keep the output out of the issue; it goes to Loki only.
  - `--timeout=<duration>`: runtime limit, such as `20m`. Defaults to the
    host's `default_timeout`, capped at `max_timeout`.
- If a command contains ```` ``` ````, use a longer fence (````` ```` `````).
- Commands must be printable ASCII. Tabs and newlines are fine. Anything
  else is rejected.

If something is wrong, the named host replies once with the reason and
creates no job. Comments addressed to other hosts are ignored.

### Good practice

- Explain what the command does and why **in a separate comment before
  it**. The operator approves from a phone.
- Keep commands short and readable. Prefer several small jobs to one long
  script, and avoid encoded payloads: they are flagged and less likely to
  be approved.
- Use `--private` whenever output could contain secrets.
- Don't edit your `/run` comment, which changes nothing, and never edit
  the job card, which voids the job. To change a command, post a new `/run`.
- Wait for the result comment before acting on the outcome. If Forgejo was
  down when the job finished, the comment is posted once it's back. Rejected,
  expired or voided jobs did not run.
