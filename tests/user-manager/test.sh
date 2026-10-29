#!/bin/bash
# Keep run_as_user in sync with internal/executor Systemd.Args.
set -euxo pipefail

helper=/usr/libexec/peon/stop-user-unit
test -x "$helper"

user=peontest
id "$user" >/dev/null 2>&1 || useradd -m "$user"
loginctl enable-linger "$user"
uid="$(id -u "$user")"
for _ in $(seq 60); do
    systemctl is-active --quiet "user@$uid.service" && break
    sleep 1
done
systemctl is-active "user@$uid.service"

# On failure, show what the user manager and SELinux saw: a stop denied by
# SELinux only says "Access denied" in systemctl's output.
diagnose() {
    set +e
    getenforce
    if command -v ausearch >/dev/null; then
        ausearch -m AVC,USER_AVC -ts recent
    fi
    journalctl --no-pager -n 50 _UID="$uid"
    journalctl --no-pager -n 50 -u 'peon-job-*'
}
trap diagnose ERR

# run_as_user UNIT COMMAND
run_as_user() {
    local unit="$1" cmd="$2" rundir="/run/user/$uid"
    systemd-run --quiet \
        --unit="$unit" --description="peon job test (as $user)" \
        --uid="$user" --property=WorkingDirectory=~ --service-type=exec \
        --property=RuntimeMaxSec=300 \
        --setenv=XDG_RUNTIME_DIR="$rundir" \
        --setenv=DBUS_SESSION_BUS_ADDRESS="unix:path=$rundir/bus" \
        "--property=ExecStopPost=-$helper $unit" \
        --collect --wait --pipe -- \
        /usr/bin/systemd-run --user --quiet \
        --unit="$unit" --description="peon job test" --service-type=exec \
        --property=RuntimeMaxSec=300 --collect --wait --pipe -- \
        /bin/bash --noprofile --norc -c "$cmd"
}

user_systemctl() {
    runuser -u "$user" -- env XDG_RUNTIME_DIR="/run/user/$uid" systemctl --user "$@"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Runs as the user, in their home, inside their user manager. The stop
# helper runs after every job; when the job already finished it must stay
# quiet, or its output would end up in every job's result.
test "$(run_as_user peon-job-t1.service 'id -un' 2>"$tmp/t1.err")" = "$user"
if [ -s "$tmp/t1.err" ]; then
    echo "unexpected stderr from a successful job:" >&2
    cat "$tmp/t1.err" >&2
    diagnose
    exit 1
fi
test "$(run_as_user peon-job-t2.service 'pwd')" = "$(getent passwd "$user" | cut -d: -f6)"
test "$(run_as_user peon-job-t3.service 'systemctl --user show-environment >/dev/null && echo ok')" = ok
cgroup="$(run_as_user peon-job-t4.service 'cat /proc/self/cgroup')"
grep -q "user@$uid.service" <<< "$cgroup"

# Rootless podman gets the systemd cgroup manager, not the cgroupfs fallback.
test "$(run_as_user peon-job-t5.service 'podman info --format "{{.Host.CgroupManager}}"')" = systemd

# Exit codes come through both layers, and a failed job is quiet too.
set +e
run_as_user peon-job-t6.service 'exit 7' 2>"$tmp/t6.err"
rc=$?
set -e
test "$rc" -eq 7
test ! -s "$tmp/t6.err"

# Stopping the outer unit (👎, timeout, shutdown) also stops the inner one.
run_as_user peon-job-t7.service 'sleep 300' 2>"$tmp/t7.err" &
pid=$!
for _ in $(seq 30); do
    user_systemctl is-active --quiet peon-job-t7.service && break
    sleep 1
done
user_systemctl is-active peon-job-t7.service
systemctl stop peon-job-t7.service
wait "$pid" || true
for _ in $(seq 30); do
    user_systemctl is-active --quiet peon-job-t7.service || break
    sleep 1
done
if user_systemctl is-active --quiet peon-job-t7.service; then
    echo "inner unit still running after the outer unit was stopped" >&2
    cat "$tmp/t7.err" >&2
    diagnose
    exit 1
fi
if grep -q "Access denied\|Failed to stop" "$tmp/t7.err"; then
    echo "the stop helper reported an error:" >&2
    cat "$tmp/t7.err" >&2
    diagnose
    exit 1
fi
