# Operations

## Logs

```
journalctl -u peon            # the daemon
journalctl -u 'peon-job-*'    # system-level job units (output goes to Loki, not here)
```

peon logs every transition: `job proposed`, `job approved`, `job started`,
`job finished`, and `job ended without running` for rejected, expired and
voided jobs. Warnings say what will be retried.

## Files

| Path | What it is |
|---|---|
| `/etc/peon/config.yaml` | configuration (`root:root 0600`) |
| `/etc/peon/forgejo.token`, `/etc/peon/loki.password` | secrets (`0600`) |
| `/var/lib/peon/state.json` | jobs, comment cursors, handled comments |
| `/var/lib/peon/state.json.lock` | prevents two peon processes sharing the state |
| `/var/lib/peon/spool/` | audit records waiting for Loki |

`state.json` is replaced atomically on every change. If peon can't parse
it, it refuses to start rather than guess. Finished jobs and handled
comments are forgotten after 30 days.

Don't edit `state.json` while peon runs. If you must, stop peon first.

## Loki outages

- **Before a job runs**, peon fails closed. It won't record an approval or
  start a job until Loki has the `approved` / `started` record. It retries
  every poll; if Loki is still down when the job expires, the job expires.
- **While a job runs**, an outage doesn't stop it. Output and other events
  spool to `/var/lib/peon/spool/` and replay in order once Loki is back.
- A spool file that can't be parsed is renamed to `*.corrupt` and kept for
  you to inspect; peon never deletes audit data silently.

The spool has no size limit. If Loki is down for a long time during busy
jobs, watch `/var/lib/peon/spool/`.

## Forgejo outages

peon keeps retrying. Waiting cards are re-checked once Forgejo is back,
and expire normally. A result or status comment that fails to post is not
retried, but the outcome is always in Loki and the daemon log.

## Upgrades and restarts

The RPM never restarts peon on upgrade, because a restart could interrupt
a running job. After upgrading, restart it when no job is running:

```
journalctl -u peon -n 20      # check nothing is running
systemctl restart peon
```

If peon is stopped or restarted while a job runs, the job is stopped and
reported **interrupted**. It is never run again. Jobs that were approved
but not yet started survive a restart, and still start only before their
expiry.

## Rotating credentials

peon reads its secrets at startup.

- **Forgejo token** (shared by all hosts): create a new token for the
  `peon` user, write it to `/etc/peon/forgejo.token` on every host,
  restart peon on each, then revoke the old token.
- **Loki password** (per host): update the proxy, then
  `/etc/peon/loki.password`, then restart peon on that host.

## Changing which repos are watched

Adding a repo is safe: on first sight of it, peon ignores existing `/run`
comments. Removing a repo stops peon watching it.

Removing a repo and later adding it back is **not** treated as new: peon
resumes from where it stopped and proposes `/run` comments posted in
between. They still need your 👍.

## Troubleshooting

**Every approval comes back as "not run" (tampered).** Forgejo may change
the card's `updated_at` when a reaction is added. That's SPEC.md open
question Q1, not yet confirmed on a real instance. Please report it with
the peon version and Forgejo version.

**A `--user` job fails with "no lingering systemd user manager".** Run
`loginctl enable-linger <user>` on that host.

**No job card appears.**
- Check the comment is from an account in `requester_ids`; others get a
  "not allowed" reply.
- Check `host` in the config matches `/run <host>` exactly.
- Check the repo is in `forgejo.repos` and the bot can read it
  (`journalctl -u peon` shows `scanning comments failed` otherwise).
- On first start, older comments are deliberately ignored.

**"state … is locked by another peon process".** Another peon is already
running with the same state directory. Only one may run per host.

## Uninstall

```
systemctl disable --now peon
dnf remove peon
```

Your edited config is kept as `/etc/peon/config.yaml.rpmsave`. The secret
files and the contents of `/var/lib/peon` are left behind for you to keep
or delete.
