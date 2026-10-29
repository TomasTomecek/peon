#!/bin/bash
# Smoke tests against the installed RPM. Run by Packit via tmt.
set -euxo pipefail

peon version

# Permissions from the package.
test "$(stat -c '%U:%G %a' /etc/peon)" = "root:root 700"
test "$(stat -c '%U:%G %a' /etc/peon/config.yaml)" = "root:root 600"
test "$(stat -c '%U:%G %a' /var/lib/peon)" = "root:root 700"
test "$(stat -c '%U:%G %a' /usr/libexec/peon/stop-user-unit)" = "root:root 755"

# status works on a fresh install, before the daemon ever ran.
peon status | grep -q "Running"
peon status --json | grep -q '"queued": \[\]'

# The unit file must parse cleanly.
systemd-analyze verify /usr/lib/systemd/system/peon.service

# The shipped example must be rejected until it is filled in.
if peon check-config; then
    echo "placeholder config unexpectedly passed validation" >&2
    exit 1
fi

# A filled-in config must pass.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
( umask 077; echo token > "$tmp/forgejo.token"; echo password > "$tmp/loki.password" )
sed -e 's/^host: .*/host: smoke/' \
    -e 's/^  bot_user_id: .*/  bot_user_id: 31/' \
    -e 's/^  approver_id: .*/  approver_id: 1/' \
    -e 's/^  requester_ids: .*/  requester_ids: [7]/' \
    -e "s|/etc/peon/forgejo.token|$tmp/forgejo.token|" \
    -e "s|/etc/peon/loki.password|$tmp/loki.password|" \
    /etc/peon/config.yaml > "$tmp/config.yaml"
peon --config "$tmp/config.yaml" check-config

# Secret files that others can read are refused.
chmod 0644 "$tmp/forgejo.token"
if peon --config "$tmp/config.yaml" check-config; then
    echo "world-readable token unexpectedly accepted" >&2
    exit 1
fi
