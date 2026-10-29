# peon — specification

> *Zug zug.* A per-host daemon that runs commands proposed in Forgejo issues, only after explicit human approval, with every step audited in Loki.

Status: draft v0.1

---

## 1. Purpose

Agents communicate with the operator through Forgejo issues. Some tasks need a state-changing or privileged command run on a specific server. Read-only commands are already handled by a separate, guarded agent path. **peon** handles everything else, and only with human approval.

The operator must be able to approve these commands from a phone, without being at a computer.

### Goals

- Free-form shell commands, including root, on any enrolled host.
- Approval by a single 👍 reaction from the operator.
- Approval is cryptographically bound to the exact command, host, and job. Editing anything after approval invalidates it.
- Single-use, time-limited approvals.
- Full audit trail in Loki, written **before** execution (fail closed).
- Results reported back to the issue, with a Grafana link.
- No inbound network ports on hosts. Daemons pull from Forgejo.

### Non-goals

- Interactive commands (no stdin, no TTY).
- File transfer.
- Fan-out to multiple hosts in one job. One job targets exactly one host.
- Replacing the existing read-only agent tooling. peon shares no credentials with it.
- Defending against a compromised operator Forgejo account (see §2).

---

## 2. Threat model

| Threat | Mitigation |
|---|---|
| Prompt-injected or compromised agent proposes a malicious command | Human review is the control. The bot comment shows the command verbatim and flags hard-to-read content (§5.3). |
| Agent edits a comment after (or while) the operator reads it | The command is executed from the daemon's local copy, never from Forgejo. The bot comment must be unedited at approval time (§6). |
| Agent spoofs a bot proposal comment | Only comments authored by the peon bot **user ID** are considered, and only for job IDs that exist in the daemon's local state. A card a daemon didn't create is ignored. |
| Agent reacts 👍 itself | Only reactions from the configured approver **user ID** count. |
| Replay of an old approval | Job IDs are single-use and enforced in local state. Approvals expire. |
| Root command erases its own audit trail | The start record is pushed to Loki before execution. The daemon's Loki credential is push-only. |
| Secrets leaked into issues via command output | `--private` flag: output goes to Loki only (§8.2). |
| Bugs in the network-facing daemon | The daemon runs unprivileged and heavily sandboxed. Root execution goes through systemd via a narrow polkit rule (§7.1). This limits damage from bugs (path traversal, stray writes). It does **not** contain an attacker with code execution in the daemon: whoever can start a root transient unit is root-equivalent. See Q2. |
| Compromise of one host (and so the shared bot token) | The bot is not a requester, so its token cannot create jobs on any host; jobs exist only from a `/run` by an allowed requester. A stolen token **can** edit other hosts' job cards (they become `tampered`, which is a denial of service) and post fake result comments. Loki is the authoritative record of what ran, not the issue. Revoking the token affects every host. |
| Operator Forgejo account compromise | **Out of scope.** Forgejo has no public route. The operator token/session must never be given to any agent. |

---

## 3. Identities

| Identity | Description |
|---|---|
| **Approver** | The operator's Forgejo user. Configured by numeric **user ID**, not username. |
| **Requesters** | Allowlist of Forgejo user IDs allowed to propose jobs (agents, and optionally the operator). |
| **Bot** | One Forgejo user, `peon`, shared by every host. Posts proposal and result comments. Its token is scoped to issue read/write on watched repos only, and it must never be in `requester_ids`. Cards and results always name the host, because the author no longer does. |

---

## 4. Command syntax

An issue comment by a requester:

````
/run db1 [--private] [--timeout=10m]
```sh
systemctl restart postgresql
```
````

Rules:

- The first line must start with `/run <host>`. `<host>` must equal the daemon's configured `host` exactly. Every other daemon ignores the comment.
- Exactly one fenced code block follows. The language tag is optional (`sh`/`bash` or none).
- Flags:
  - `--private`: no output is posted to the issue (§8.2).
  - `--timeout=<duration>`: default `10m`, capped by config `max_timeout`.
- Anything else is a parse error. The bot replies with the error and creates no job.

---

## 5. Proposal

### 5.1 Job record

When a valid `/run` is seen, the daemon creates a job in local state:

```json
{
  "job_id": "01JB…",               // ULID
  "host": "db1",
  "repo": "owner/name",
  "issue": 42,
  "source_comment_id": 1234,
  "requester_id": 7,
  "requester": "agent-ops",
  "command": "systemctl restart postgresql",
  "flags": { "private": false, "timeout": "10m" },
  "created_at": "2026-10-29T10:00:00Z",
  "expires_at": "2026-10-29T10:30:00Z"
}
```

`job_hash = sha256(canonical_json(job_record))`. Canonical JSON uses sorted keys, no whitespace, and UTF-8.

### 5.2 Bot comment

The bot posts a comment on the issue:

````
🪓 peon job `01JB…` on **db1**

Requested by @agent-ops (from comment #1234)
Expires 10:30 UTC · timeout 10m · output: posted

```sh
systemctl restart postgresql
```

hash `3f9a…c21e` (first 12 shown)

👍 to approve · 👎 to reject
````

The daemon stores the exact bytes of the comment body it posted, and the comment ID.

### 5.3 Command validation

Hard rejects (no job is created):

- Any byte outside printable ASCII, `\n`, and `\t`. This rejects invisible Unicode, homoglyphs, and `\r`.
- Length over `max_command_bytes` (default 4096).
- Empty command.

Warnings (the job is created, and the warning is shown prominently above the command block):

- More than one line.
- Any line over 80 characters (hard to read on a phone).
- Patterns: `base64`, `curl … | sh`, `wget … | sh`, `eval`, `xxd`, `\x` escapes, `rm -rf /`, writes to `/dev/sd*`, `mkfs`, `dd`.

Warnings do not block. The operator decides.

---

## 6. Approval

The daemon polls reactions on the open job's bot comment. A job is approved only if **all** of these hold at the moment of the check:

1. A 👍 (`+1`) reaction exists from `approver_id`.
2. No 👎 (`-1`) reaction from `approver_id` exists.
3. The bot comment's current body matches the stored bytes exactly.
4. The bot comment was never edited (`updated_at == created_at`; see open question Q1).
5. The comment author is the bot user ID, and the job ID on the card exists in local state.
6. `now < expires_at`.
7. The job state is `proposed` (single-use).

If check 3 or 4 fails, the job becomes `tampered`, which is terminal. The bot posts a warning, and the agent must propose again.

A 👎 from the approver moves the job to `rejected` (terminal).

**The command executed is always `job_record.command` from local state. Forgejo content is never executed.**

---

## 7. Execution

### 7.1 Privilege separation

- `peon.service` runs as an unprivileged user `peon`.
- A polkit rule allows user `peon` to start and stop transient units whose names start with `peon-job-`, and nothing else (see open question Q2).
- Jobs run as root in a transient unit:

```
systemd-run \
  --unit=peon-job-<job_id> \
  --uid=root \
  --working-directory=/ \
  --property=RuntimeMaxSec=<timeout> \
  --property=StandardInput=null \
  --collect --wait --pipe \
  /bin/bash --noprofile --norc -c "<command>"
```

- The environment is clean (systemd default). No agent-controlled environment variables.
- **One job at a time per host.** Approved jobs queue in FIFO order.

### 7.2 Cancellation

- 👎 from the approver on a **running** job triggers `systemctl stop peon-job-<job_id>`. The state becomes `killed`.
- A timeout also produces `timed_out`.

### 7.3 Job states

```
proposed ──👍──▶ approved ──▶ running ──▶ succeeded | failed | timed_out | killed
   │
   ├──👎──▶ rejected
   ├──time──▶ expired
   └──edit──▶ tampered
```

All terminal states are final. A job ID can never return to `proposed`.

---

## 8. Audit and reporting

### 8.1 Loki

- Pushed via `/loki/api/v1/push` using a **push-only** credential.
- Stream labels: `job="peon"`, `host`, `job_id`, `repo`, `issue`, `requester`, `approver`, `event`.
- Events: `proposed`, `approved`, `rejected`, `expired`, `tampered`, `started`, `stdout`, `stderr`, `finished`.
- Each event line is JSON and includes `job_hash` and the full command (on `proposed` and `started`).
- **Fail closed:** if the `approved` and `started` push does not succeed, the job does not run. The daemon retries until expiry, then marks the job `expired`.
- If a push fails **during** a run, the job is not killed. Lines spool to `/var/lib/peon/spool/` and are retried in order.
- A local copy is also written to journald.

### 8.2 Result comment

Posted by the bot when the job finishes:

- Status, exit code, duration.
- Last `output_tail_lines` lines (default 50, max 4 KB) of combined output, unless `--private`.
- With `--private`: only status and exit code, plus a note that the output is in Loki only.
- A Grafana Explore link filtered by `job_id`.

---

## 9. Polling

- Poll new comments on watched repos with `GET /repos/{owner}/{repo}/issues/comments?since=<cursor>`.
- Poll reactions on open jobs with `GET /repos/{owner}/{repo}/issues/comments/{id}/reactions`.
- Default interval: 10 s, with jitter.
- The cursor is persisted in local state.

---

## 10. Configuration

`/etc/peon/config.yaml` (root:peon, 0640)

Parsing is strict: unknown keys, duplicate keys and multiple YAML documents are errors, and durations must be strings such as `30m` (a bare `30` is rejected as ambiguous).

```yaml
host: db1

forgejo:
  url: https://forgejo.internal
  repos:
    - ops/infra
    - ops/agents
  bot_user_id: 31
  bot_token_file: /etc/peon/forgejo.token   # 0600 peon; same token on every host
  approver_id: 1
  requester_ids: [7, 8, 9]

limits:
  job_ttl: 30m
  default_timeout: 10m
  max_timeout: 2h
  max_command_bytes: 4096
  output_tail_lines: 50
  poll_interval: 10s

loki:
  url: https://loki.internal
  user: peon-db1
  password_file: /etc/peon/loki.password    # 0600 peon

grafana:
  explore_url: https://grafana.internal/explore
```

Local state: SQLite at `/var/lib/peon/peon.db`, owned by `peon`, with a unique `job_id` and state transitions enforced in transactions.

---

## 11. Implementation

- Go, a single static binary.
- `peon.service` with systemd hardening on the daemon: `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `ReadWritePaths=/var/lib/peon`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, and so on. These apply to the daemon only, not the job units.
- `peon version` and `peon check-config` subcommands. `check-config` runs as `ExecStartPre`, so a bad config stops the service before it polls anything.
- Packaged as an RPM (`peon.spec`) for Fedora and EPEL 10. Packit builds it in Copr for every pull request and every commit to `main`, and runs tmt smoke tests against the installed package. The source tarball vendors Go modules so builds are offline.

---

## 12. Open questions

- **Q1.** Does adding a reaction change a comment's `updated_at` in Forgejo? If it does, check 4 needs another approach, such as Forgejo's comment content-history, or relying on body comparison alone.
- **Q2.** Verify the polkit rule can match on transient unit names for `StartTransientUnit` (`action.lookup("unit")`). The shipped rule (`dist/50-peon.rules`) fails closed if it can't. Either way, the `peon` user is root-equivalent, because it chooses the command a root unit runs. If that matters, the stronger design is a split: a tiny root helper that verifies the approval itself (re-reading the Forgejo comment and reactions with its own read-only token) before running anything, so a compromised daemon cannot run unapproved commands.
- **Q3.** Should the operator be able to edit a command before approving (for example, reply `/amend` with a new block), or must the agent always re-propose? Current answer: re-propose. Simpler and safer.
- **Q4.** Loki tenancy: one tenant for all peons, or per host?
