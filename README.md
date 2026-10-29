# peon

*Zug zug.*

A per-host daemon that runs commands proposed by agents in Forgejo issues,
**only after you approve them with a 👍**, and records every step in Loki.

Agents post:

````
/run db1
```sh
systemctl restart postgresql
```
````

Add `--user=deploy` to run in that user's systemd user manager instead of
as root (for rootless podman and `systemctl --user`).

peon on `db1` replies with a job card showing the exact command and its
hash. You react 👍 on that card from your phone; peon runs the command as
root in a transient systemd unit, ships the output to Loki, and posts the
result with a Grafana link. 👎 rejects, or kills a running job.

The full design and threat model are in [SPEC.md](SPEC.md).

## Status

Early, but complete end to end: `peon run` watches Forgejo, posts job
cards, applies the approval rules, runs approved jobs in transient systemd
units and audits everything in Loki. Not yet tried against a real Forgejo
instance. Requires systemd 257 or newer (Fedora 43+, EL10).

## Install

RPMs are built by [Packit](https://packit.dev) in Copr on every commit to
`main`:

```
dnf copr enable <owner>/peon    # Fedora and EPEL 10
dnf install peon
```

Then:

1. Once, for all hosts: create a `peon` bot account in Forgejo and a token
   with issue read/write on the watched repos.
2. Write the token to `/etc/peon/forgejo.token` and the Loki push password
   to `/etc/peon/loki.password`, both `root:root` mode `0600`.
3. Fill in `/etc/peon/config.yaml` and run `peon check-config`.
4. `systemctl enable --now peon`

## Development

```
go test ./...
go build ./cmd/peon
```

Packaging: `peon.spec`, `.packit.yaml`, and `packaging/create-archive.sh`,
which builds a source tarball with vendored Go modules. Smoke tests in
`tests/smoke/` run against the installed RPM via tmt on every pull request.
