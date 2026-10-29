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
| Bugs in the network-facing daemon | The daemon runs as root but with **no capabilities** and a tight systemd sandbox (read-only system, no devices, restricted syscalls), so a bug such as a stray write cannot touch the host directly. It does **not** contain an attacker with code execution in the daemon: it can ask systemd to start a root unit. The approval flow is the control, not the sandbox. A polkit-based unprivileged daemon was designed and dropped for simplicity; see Q2. |
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
/run db1 [--user=postgres] [--private] [--timeout=10m]
```sh
systemctl restart postgresql
```
````

Rules:

- The first line must start with `/run <host>`. `<host>` must equal the daemon's configured `host` exactly. Every other daemon ignores the comment.
- Exactly one fenced code block follows. The language tag is optional (`sh`/`bash` or none).
- Flags:
  - `--user=<name>`: run as this user instead of `jobs.default_user` (§7.4).
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
  "user": "root",
  "command": "systemctl restart postgresql",
  "flags": { "private": false, "timeout": "10m0s" },
  "created_at": "2026-10-29T10:00:00Z",
  "expires_at": "2026-10-29T10:30:00Z"
}
```

`job_hash = sha256(canonical_json(job_record))`. Canonical JSON uses sorted keys, no whitespace, and UTF-8; times are RFC 3339 UTC with second precision, and the timeout is a Go duration string (`10m0s`).

### 5.2 Bot comment

The bot posts a comment on the issue:

````
🪓 peon job `01JB…` on **db1**

Requested by @agent-ops (from comment #1234)
Runs as root · timeout 10m · output: posted
Expires 10:30 UTC

```sh
systemctl restart postgresql
```

hash `3f9a…c21e` (first 12 shown)

👍 to approve · 👎 to reject
````

The command block uses a backtick fence longer than any backtick run in the command, so a command containing ```` ``` ```` cannot close the block early and make text outside it look like part of the card.

The daemon stores the comment ID, the body and `updated_at` exactly as Forgejo returned them on creation. If the returned body differs from what peon sent, the job is voided as `tampered` immediately.

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
4. The bot comment's `updated_at` still equals the value Forgejo returned when it was posted. An edit that restores the original text is still caught (see open question Q1).
5. The comment author is the bot user ID. Daemons only ever look at cards of jobs in their own local state.
6. `now < expires_at`.
7. The job state is `proposed` (single-use).

If check 3, 4 or 5 fails, or the card was deleted, the job becomes `tampered`, which is terminal. The bot posts a warning, and the agent must propose again. Integrity is checked before anything else, then 👎, then expiry, then 👍.

A 👎 from the approver moves the job to `rejected` (terminal).

**The command executed is always `job_record.command` from local state. Forgejo content is never executed.**

---

## 7. Execution

### 7.1 Running jobs

- `peon.service` runs as root with an empty capability bounding set, `NoNewPrivileges=yes`, `ProtectSystem=strict` and the other sandboxing in the unit file. systemd lets it manage units because its effective UID is 0, which systemd checks by UID alone since **systemd 257**; the RPM requires it.
- Jobs run as root in a transient unit:

```
systemd-run --quiet \
  --unit=peon-job-<job_id>.service \
  --description="peon job <job_id>" \
  --uid=root \
  --working-directory=/ \
  --service-type=exec \
  --property=RuntimeMaxSec=<timeout> \
  --collect --wait --pipe \
  -- /bin/bash --noprofile --norc -c "<command>"
```

- The command is a single argv element; peon never interpolates it into a shell itself. stdin is `/dev/null`.
- The environment is clean (systemd default). No agent-controlled environment variables.
- **One job at a time per host.** Approved jobs queue in approval order (request order within one poll).
- An approval is for running the job soon: a job that has not **started** by `expires_at`, for example because a long job was running, expires instead.
- The job is marked `running` **on disk** before the command starts. If that write fails, it does not start.

### 7.2 Cancellation

- 👎 from the approver on a **running** job triggers `systemctl stop peon-job-<job_id>`. The state becomes `killed`.
- A timeout also produces `timed_out`. systemd-run does not say why a unit ended, so a non-zero exit at or past the timeout counts as a timeout. peon also stops the unit itself 30 s past the timeout, in case `RuntimeMaxSec` did not fire.
- If peon is stopped while a job runs, it stops the job and reports it `interrupted`.
- If peon finds a job still `running` in its state at startup (it crashed), it stops the unit, reports the job `interrupted`, and **never runs it again**.

### 7.3 Job states

```
proposed ──👍──▶ approved ──▶ running ──▶ succeeded | failed | timed_out | killed | interrupted
   │
   ├──👎──▶ rejected
   ├──time──▶ expired
   └──edit──▶ tampered
```

All terminal states are final. A job ID can never return to `proposed`.

---

### 7.4 Running as other users

Jobs run as `jobs.default_user` (default `root`), or as the user named with `--user=<name>`, which must be listed in `jobs.allowed_users` (default `[root]`). The user is part of the job record, so it is covered by the hash, and it is shown prominently on the card.

Non-root jobs run **inside that user's systemd user manager**, so they get the user's real session: `XDG_RUNTIME_DIR`, the user bus, cgroup delegation and their home as the working directory. Rootless podman (with the systemd cgroup manager) and `systemctl --user` work as if the user had logged in. To get there without extra privileges, a system unit running as the user starts the command with `systemd-run --user`:

```
systemd-run --quiet --unit=peon-job-<id>.service --uid=<user> \
  --property=WorkingDirectory=~ --service-type=exec --property=RuntimeMaxSec=<timeout> \
  --setenv=XDG_RUNTIME_DIR=/run/user/<uid> \
  --setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus \
  --property="ExecStopPost=-/usr/bin/systemctl --user stop peon-job-<id>.service" \
  --collect --wait --pipe -- \
  /usr/bin/systemd-run --user --quiet --unit=peon-job-<id>.service --service-type=exec \
  --property=RuntimeMaxSec=<timeout> --collect --wait --pipe -- \
  /bin/bash --noprofile --norc -c "<command>"
```

Stopping the outer system unit (👎, timeout, shutdown) stops the inner user unit through `ExecStopPost`. Exit codes pass through both layers.

Requirements and checks:

- The user's manager must run with nobody logged in: **lingering** (`loginctl enable-linger <user>`). `check-config` and startup refuse a non-root allowed user without it, and so does each job start. peon never enables lingering itself.
- Users must exist. peon resolves them from `/etc/passwd` only (the binary is static, without NSS), so users from LDAP/SSSD are not supported yet.
- This is a convenience, not a security boundary: a requester can still pick `root` if it is allowed. The operator's review is the control.

## 8. Audit and reporting

### 8.1 Loki

- Pushed via `/loki/api/v1/push` using a **push-only** credential.
- Stream labels: `job="peon"`, `host`, `job_id`, `repo`, `issue`, `requester`, `approver`, `event`.
- Events: `proposed`, `approved`, `rejected`, `expired`, `tampered`, `started`, `kill_requested`, `stdout`, `stderr`, `finished`.
- Lifecycle event lines are JSON and include `job_hash`, the state, the requester, the approver, and the full command (on `proposed` and `started`). `stdout` / `stderr` lines are the raw output lines (capped at 64 KB each).
- **Fail closed:** the job is not approved until the `approved` record is in Loki, and does not start until the `started` record is. The daemon retries every poll; if Loki is still down at the job's expiry, the job becomes `expired`. Other events are spooled when Loki is down.
- If a push fails **during** a run, the job is not killed. Lines spool to `/var/lib/peon/spool/` and are retried in order.
- The daemon also logs every transition to journald.

### 8.2 Result comment

Posted by the bot when the job finishes:

- Status, exit code, duration.
- Last `output_tail_lines` lines (default 50, max 4 KB) of combined output, unless `--private`.
- With `--private`: only status and exit code, plus a note that the output is in Loki only.
- A Grafana Explore link and the LogQL query `{job="peon", job_id="<id>"}`. With `grafana.loki_datasource_uid` set, the link opens the job's logs directly.

---

## 9. Polling

- Poll new comments on watched repos with `GET /repos/{owner}/{repo}/issues/comments?since=<cursor>`.
- Poll reactions on open jobs with `GET /repos/{owner}/{repo}/issues/comments/{id}/reactions`.
- Default interval: 10 s, with jitter.
- The cursor is persisted in local state and only ever holds Forgejo's own timestamps, so clock differences between peon and Forgejo cannot make it skip comments. On the very first start for a repo, peon reads the last 30 days and marks existing `/run` comments as handled: old requests are never acted on.
- Each `/run` comment for this host is handled once, by comment ID: it becomes a job or gets one error reply. Editing it later does nothing.
- Daemons ignore `/run` comments for other hosts entirely, including malformed ones, so only the named host ever replies.

---

## 10. Configuration

`/etc/peon/config.yaml` (root:root, 0600)

Parsing is strict: unknown keys, duplicate keys and multiple YAML documents are errors, and durations must be strings such as `30m` (a bare `30` is rejected as ambiguous).

```yaml
host: db1

forgejo:
  url: https://forgejo.internal
  repos:
    - ops/infra
    - ops/agents
  bot_user_id: 31
  bot_token_file: /etc/peon/forgejo.token   # root 0600; same token on every host
  approver_id: 1
  requester_ids: [7, 8, 9]

jobs:
  default_user: root
  allowed_users: [root, deploy]   # non-root users need lingering (§7.4)

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
  password_file: /etc/peon/loki.password    # root 0600

grafana:
  explore_url: https://grafana.internal/explore
  loki_datasource_uid: P8E80F9AEF21F6940   # optional
```

Local state: `/var/lib/peon/state.json`, owned by `peon`, replaced atomically (write, fsync, rename, fsync directory) on every change, and locked with `flock` so two daemons can never share it. A state file peon cannot parse stops it from starting. Loki spool: `/var/lib/peon/spool/`. Finished jobs are kept for 30 days.

---

## 11. Implementation

- Go, a single static binary.
- `peon.service` with systemd hardening on the daemon: `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `ReadWritePaths=/var/lib/peon`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, and so on. These apply to the daemon only, not the job units.
- `peon version` and `peon check-config` subcommands. `check-config` runs as `ExecStartPre`, so a bad config stops the service before it polls anything.
- Packaged as an RPM (`peon.spec`) for Fedora and EPEL 10. Packit builds it in Copr for every pull request and every commit to `main`, and runs tmt smoke tests against the installed package. The source tarball vendors Go modules so builds are offline.

---

## 12. Open questions

- **Q1.** Does adding a reaction change a comment's `updated_at` in Forgejo? If it does, check 4 needs another approach, such as Forgejo's comment content-history, or relying on body comparison alone.
- **Q2.** ~~Unprivileged daemon via polkit?~~ **Dropped for now.** It would run the daemon as a `peon` user with a polkit rule allowing only `peon-job-<ULID>.service` start/stop (systemd ≥ 257 passes the unit name to polkit for transient units; older releases pass none). It only contains bugs, not attackers, since the `peon` user chooses what root units run. If that is ever wanted, the stronger design is a small root helper that re-verifies the approval in Forgejo itself before running anything.
- **Q3.** Should the operator be able to edit a command before approving (for example, reply `/amend` with a new block), or must the agent always re-propose? Current answer: re-propose. Simpler and safer.
- **Q4.** Loki tenancy: one tenant for all peons, or per host?
