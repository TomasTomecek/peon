# AGENTS.md

Instructions for AI agents working in this repository. Humans are welcome
too.

peon runs commands **as root** on servers after a human approves them with
a 👍 in Forgejo. A bug here is usually a security bug. Read
[SPEC.md](SPEC.md) before changing behaviour: it is the source of truth,
and §2 is the threat model.

## Layout

| Path | What it is |
|---|---|
| `cmd/peon` | CLI: `version`, `check-config`, `run` |
| `internal/command` | Parses `/run` comments and validates commands |
| `internal/job` | Job record, hash, state machine, rendered comments, approval rules |
| `internal/daemon` | The poll loop: proposals, approval, queue, execution, audit |
| `internal/executor` | Runs jobs with `systemd-run` |
| `internal/forgejo` | Minimal Forgejo REST client |
| `internal/loki` | Loki push client and on-disk spool |
| `internal/state` | Atomic, locked JSON state file |
| `internal/config` | Strict YAML config loading and validation |
| `dist/` | systemd unit and example config, installed by the RPM |
| `peon.spec`, `.packit.yaml`, `packaging/` | RPM packaging and Packit/Copr builds |
| `plans/`, `tests/` | tmt tests run by Packit on Testing Farm against the installed RPM |

## Build and test

```
go build ./cmd/peon
go vet ./...
go test ./...
go test -race ./...   # before opening a PR
gofmt -l .            # must print nothing
```

- Unit tests must not need the network, root or systemd. Use fakes: the
  daemon tests drive the whole loop tick by tick against in-memory
  Forgejo, Loki and executor fakes (`internal/daemon/fakes_test.go`) with
  a controllable clock. New daemon behaviour gets a scenario test there.
- Anything that needs real systemd, users or podman goes into a tmt test
  under `tests/`.
- `go.mod` says Go 1.22, so don't use newer standard library APIs (for
  example `testing.T.Context`). Fedora and EPEL build with their own Go.

## Security invariants

Each of these has tests. Do not weaken one without the operator agreeing
in the issue first; if a task seems to require it, stop and ask.

1. **Nothing from Forgejo is ever executed.** The command that runs is
   `job.Record.Command` from local state, captured when the job was
   proposed.
2. **Approval** is a 👍 from `approver_id`, matched by user ID and never
   by login, on an unedited card (body *and* `updated_at` as Forgejo
   returned them on creation), before expiry. The order is integrity, then
   👎, then expiry, then 👍. This lives in `job.Evaluate`; keep it a pure
   function.
3. **The hash covers everything the approval covers.** If you add a field
   to `job.Record`, add it to `Record.Canonical()`, the canonical JSON
   test, `TestHashCoversEveryField`, and the job card.
4. **Fail closed.** The `approved` and `started` records must reach Loki
   before peon acts on them. A job's `running` state must be saved to disk
   before its command starts.
5. **Single use.** A job never goes back to an earlier state, and is never
   re-run after a crash or restart.
6. **What you see is what runs.** Commands are printable ASCII only. Any
   user-controlled text in a comment goes inside a code block fenced with
   `job.Fence`, so it cannot break out of the block.
7. **No shell building.** The command is passed to `bash -c` as a single
   argv element. Never interpolate input into a shell string or a unit
   property.
8. **Strict config.** Unknown and duplicate keys are errors, and secret
   files must be mode 0600.
9. **Secrets stay out of issues.** Output of `--private` jobs never goes
   to Forgejo. Never log tokens or passwords.

## Changing behaviour

- Update `SPEC.md` in the same PR. Security-relevant changes also update
  the threat model in §2.
- The daemon runs as root with an empty capability set and a tight
  sandbox (`dist/peon.service`). Keep it that way. Jobs run in separate
  transient units, which that hardening does not apply to.
- When you change file paths or permissions, change `peon.spec`,
  `dist/`, the README and `tests/smoke/test.sh` together.

## Dependencies

Avoid new dependencies; the only one today is `gopkg.in/yaml.v3`. If one is
truly needed, justify it in the PR. It must vendor cleanly:
`packaging/create-archive.sh` vendors modules so the RPM builds offline.

## Commits and pull requests

- One focused PR per change, based on `main`. Don't stack PRs on other
  feature branches: GitHub only retargets a stacked PR if its base branch
  is deleted, otherwise it merges into the wrong branch (this happened
  with #4).
- Never push to `main` directly.
- Commit subjects are imperative and short. The body explains why.

## Requesting commands through peon

This section is for agents that *use* peon on the servers it runs on,
rather than develop it.

- Post one comment per command: `/run <host>` on the first line, then
  exactly one fenced code block and nothing else. Flags are listed in
  SPEC.md §4.
- Explain in a separate, preceding comment what the command does and why
  it is needed. The operator approves from a phone, so keep commands
  short and readable, and avoid encoded payloads (`base64`, `curl … | sh`).
  peon flags those, and they are much less likely to be approved.
- Use `--private` whenever the output could contain secrets.
- Don't edit a `/run` comment or a job card. Editing changes nothing, and
  editing a card voids the job. To change a command, post a new `/run`.
- Wait for peon's result comment before acting on the outcome. An expired
  or rejected job did not run.
