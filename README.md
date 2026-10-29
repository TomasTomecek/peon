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

peon on `db1` replies with a job card showing the exact command and its
hash. You react 👍 on that card from your phone; peon runs the command as
root in a transient systemd unit, ships the output to Loki, and posts the
result with a Grafana link. 👎 rejects, or kills a running job.

The full design and threat model are in [SPEC.md](SPEC.md).

## Status

Early. Implemented: configuration loading and validation, `peon version`,
`peon check-config`, systemd unit, polkit rule, RPM packaging.
Not yet implemented: the daemon loop (`peon run` exits with an error).

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
   to `/etc/peon/loki.password`, both `peon:peon` mode `0600`.
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
