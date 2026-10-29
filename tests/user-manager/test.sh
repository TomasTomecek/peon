#!/bin/bash
# Keep run_as_user in sync with internal/executor Systemd.Args.
#
# Output of run_as_user is only ever captured through pipes, as peon does.
# systemd-run --pipe hands its stdio to PID 1, and SELinux doesn't let
# init_t write files in /tmp (user_tmp_t): redirecting to a file there
# makes systemd-run fail with "Bad message".

# Trace to a separate descriptor, so "2>&1" below captures only what the
# job and systemd-run print, not the shell's own trace.
exec 9>&2
BASH_XTRACEFD=9
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

# On failure, show what SELinux and the journal saw: systemctl reports
# every denial, SELinux ones included, only as "Access denied".
diagnose() {
    set +e
    getenforce
    if command -v ausearch >/dev/null; then
        ausearch -m AVC,USER_AVC,SELINUX_ERR -ts recent
    fi
    journalctl --no-pager -n 50 _UID="$uid"
    journalctl --no-pager -n 300 | grep -F peon-job | tail -n 50
}
tmp="$(mktemp -d)"
trap 'rc=$?; if [ "$rc" -ne 0 ]; then diagnose; fi; rm -rf "$tmp"' EXIT

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

# Runs as the user, in their home, inside their user manager. stderr is
# folded into the captured output: the stop helper runs after every job and
# must stay quiet when the job already finished, or its output would end up
# in every job's result.
test "$(run_as_user peon-job-t1.service 'id -un' 2>&1)" = "$user"
test "$(run_as_user peon-job-t2.service 'pwd' 2>&1)" = "$(getent passwd "$user" | cut -d: -f6)"
test "$(run_as_user peon-job-t3.service 'systemctl --user show-environment >/dev/null && echo ok' 2>&1)" = ok
cgroup="$(run_as_user peon-job-t4.service 'cat /proc/self/cgroup' 2>&1)"
grep -q "user@$uid.service" <<< "$cgroup"

# Rootless podman gets the systemd cgroup manager, not the cgroupfs fallback.
test "$(run_as_user peon-job-t5.service 'podman info --format "{{.Host.CgroupManager}}"' 2>/dev/null)" = systemd

# Exit codes come through both layers, and a failed job is quiet too.
set +e
out="$(run_as_user peon-job-t6.service 'exit 7' 2>&1)"
rc=$?
set -e
test "$rc" -eq 7
test -z "$out"

# Stopping the outer unit (👎, timeout, shutdown) also stops the inner one.
run_as_user peon-job-t7.service 'sleep 300' 2>&1 | cat > "$tmp/t7.out" &
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
cat "$tmp/t7.out"
if user_systemctl is-active --quiet peon-job-t7.service; then
    echo "inner unit still running after the outer unit was stopped" >&2
    exit 1
fi
if grep -q "Access denied\|Failed to stop" "$tmp/t7.out"; then
    echo "the stop helper reported an error" >&2
    exit 1
fi
