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

Documentation: [setup](docs/setup.md), [using peon](docs/using.md) and
[operations](docs/operations.md). The full design and threat model are in
[SPEC.md](SPEC.md).

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

Then follow [docs/setup.md](docs/setup.md): create the Forgejo bot
account, write the secrets, fill in `/etc/peon/config.yaml`, run
`peon check-config` and `systemctl enable --now peon`.

## Development

```
go test ./...
go build ./cmd/peon
```

Contributor and agent guidelines, including the security invariants every
change must keep, are in [AGENTS.md](AGENTS.md).

Packaging: `peon.spec`, `.packit.yaml`, and `packaging/create-archive.sh`,
which builds a source tarball with vendored Go modules. On every pull
request, Testing Farm runs the Go unit tests (`tests/unit/`) and tests
against the installed RPM (`tests/smoke/`, `tests/user-manager/`) via tmt.
