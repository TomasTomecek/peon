# Setup

peon runs on every host that should accept commands. Each host watches the
same Forgejo repos, and only acts on `/run <host>` comments addressed to it.

Requirements: systemd 257 or newer (Fedora 43+, EL10), a Forgejo instance,
and a Loki that the host can push to. Grafana is optional but recommended.

## 1. Forgejo accounts (once, for all hosts)

peon needs three kinds of account, identified by **numeric user ID**:

| Account | Role | Config key |
|---|---|---|
| You | The only account whose 👍 / 👎 counts | `forgejo.approver_id` |
| The `peon` bot | Posts job cards and results; one account shared by all hosts | `forgejo.bot_user_id` |
| Your agents | Allowed to post `/run` | `forgejo.requester_ids` |

1. Create a `peon` user in Forgejo and give it at least read access to each
   repo peon should watch. Reading and commenting on issues is enough.
2. As the `peon` user, create an access token with the **`write:issue`**
   scope, plus `read:repository` if your Forgejo version requires it to list
   comments. If listing comments on a private repo returns 404, the bot
   lacks access to that repo or the token lacks a scope.
3. Look up each account's numeric ID; the web UI doesn't show it:

   ```
   curl -s -H "Authorization: token $TOKEN" https://forgejo.internal/api/v1/users/<login> | jq .id
   ```

Never give your own token or session to an agent. Anyone holding it can
approve jobs.

## 2. Loki credential (per host)

peon pushes audit records to `<loki.url>/loki/api/v1/push` with HTTP basic
auth. Loki itself has no authentication, so put it behind a reverse proxy
that:

- requires basic auth, with one user per host (`peon-db1`, …), and
- allows only `POST /loki/api/v1/push` for those users.

A push-only credential means a root command on a host can't read or delete
what peon already shipped.

peon does not send an `X-Scope-OrgID` header. If your Loki runs with
`auth_enabled: true`, have the proxy add the tenant header.

## 3. Install

```
dnf copr enable <owner>/peon
dnf install peon
```

The package installs:

| Path | |
|---|---|
| `/usr/bin/peon` | the binary |
| `/usr/lib/systemd/system/peon.service` | the daemon unit (not enabled) |
| `/etc/peon/config.yaml` | example config, `root:root 0600` |
| `/var/lib/peon/` | state, audit spool and the running job's output, `root:root 0700` |

## 4. Secrets

```
install -m 0600 /dev/null /etc/peon/forgejo.token
install -m 0600 /dev/null /etc/peon/loki.password
$EDITOR /etc/peon/forgejo.token /etc/peon/loki.password
```

Each file holds just the secret (a trailing newline is fine). peon refuses
to start if either file is readable by group or others.

## 5. Configuration

Edit `/etc/peon/config.yaml`. Every key in the example is documented
inline. The ones you must set:

```yaml
host: db1                          # what agents write in "/run db1"

forgejo:
  url: https://forgejo.internal     # without /api/v1
  repos: [ops/infra]
  bot_user_id: 31
  bot_token_file: /etc/peon/forgejo.token
  approver_id: 1
  requester_ids: [7, 8]

loki:
  url: https://loki.internal
  user: peon-db1
  password_file: /etc/peon/loki.password

grafana:
  explore_url: https://grafana.internal/explore
  loki_datasource_uid: P8E80F9AEF21F6940   # optional
```

The config is strict: an unknown key, a duplicate key or a bare number
where a duration (`30m`) is expected is an error, so a typo can't silently
disable a setting.

To find `loki_datasource_uid`, open your Loki datasource in Grafana under
Connections → Data sources; the UID is the last part of the page URL. With
it, result comments link straight to the job's logs; without it, they link
to Explore and show the query.

### Running jobs as other users

By default every job runs as root. To let agents pick another user with
`--user=<name>`:

```yaml
jobs:
  default_user: root          # used when --user is omitted
  allowed_users: [root, deploy]
```

Non-root jobs run inside that user's own systemd user manager, so rootless
podman and `systemctl --user` work. That manager has to run without anyone
logged in, so enable lingering for each non-root user:

```
loginctl enable-linger deploy
```

Users must be local (in `/etc/passwd`). LDAP/SSSD users aren't supported
yet.

### Limits

| Key | Default | Meaning |
|---|---|---|
| `limits.job_ttl` | `30m` | How long a job card can wait for approval, and the latest it can **start** |
| `limits.default_timeout` | `10m` | Runtime limit when the agent doesn't pass `--timeout` |
| `limits.max_timeout` | `2h` | Largest `--timeout` accepted |
| `limits.max_command_bytes` | `4096` | Longest command accepted |
| `limits.output_tail_lines` | `50` | Output lines in the result comment (also capped at 4 KB) |
| `limits.poll_interval` | `10s` | How often peon polls Forgejo |

## 6. Check and start

```
peon check-config
systemctl enable --now peon
journalctl -u peon -f
```

`check-config` validates the file, the secret files, and that every user
in `jobs.allowed_users` exists and is lingering. The unit runs it before
every start, so a broken config stops peon rather than half-running it.

On its first start for a repo, peon logs `watching repo; existing /run
comments are not acted on`. Old requests are never run.

## 7. Try it

Post this in an issue in a watched repo, from a requester account:

````
/run db1
```sh
uptime
```
````

Within one poll interval peon posts a job card. React 👍 on the **card**
(not on your own comment), and the result follows a few seconds later.
