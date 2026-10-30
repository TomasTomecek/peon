#!/bin/bash
# Keep run_as_user in sync with internal/executor Systemd.Args.
#
# Like peon, run_as_user gives the outer system unit no --pipe: systemd-run
# --pipe hands its stdio to PID 1, and SELinux does not let init_t use
# pipes created by another service's process. PID 1 opens the output files
# itself (StandardOutput=append:), and run_as_user prints them once the
# unit has ended. append:, not truncate:: systemd reopens the files for
# ExecStopPost=, and truncate: would empty them after the job. The files live in a root-only directory under /var/lib,
# not /tmp: init_t may not write files in /tmp (user_tmp_t). The inner
# systemd-run --user keeps --pipe; it passes on the outer unit's
# descriptors, which PID 1 opened.

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
outdir=/var/lib/peon-test-output
rm -rf "$outdir"
mkdir -m 700 "$outdir"
trap 'rc=$?; if [ "$rc" -ne 0 ]; then diagnose; fi; rm -rf "$tmp" "$outdir"' EXIT

# run_as_user UNIT COMMAND
run_as_user() {
    local unit="$1" cmd="$2" rundir="/run/user/$uid"
    local out="$outdir/$unit.stdout" err="$outdir/$unit.stderr" rc=0
    systemd-run --quiet \
        --unit="$unit" --description="peon job test (as $user)" \
        --uid="$user" --property=WorkingDirectory=~ --service-type=exec \
        --property=RuntimeMaxSec=300 \
        --setenv=XDG_RUNTIME_DIR="$rundir" \
        --setenv=DBUS_SESSION_BUS_ADDRESS="unix:path=$rundir/bus" \
        "--property=ExecStopPost=-$helper $unit" \
        "--property=StandardOutput=append:$out" \
        "--property=StandardError=append:$err" \
        --collect --wait -- \
        /usr/bin/systemd-run --user --quiet \
        --unit="$unit" --description="peon job test" --service-type=exec \
        --property=RuntimeMaxSec=300 --collect --wait --pipe -- \
        /bin/bash --noprofile --norc -c "$cmd" || rc=$?
    cat "$out" 2>/dev/null || true
    cat "$err" >&2 2>/dev/null || true
    rm -f "$out" "$err"
    return "$rc"
}

# check NAME WANT GOT: fail with both values shown.
check() {
    if [ "$3" != "$2" ]; then
        printf '%s: want %q\n%s:  got %q\n' "$1" "$2" "$1" "$3" >&2
        exit 1
    fi
}

user_systemctl() {
    runuser -u "$user" -- env XDG_RUNTIME_DIR="/run/user/$uid" systemctl --user "$@"
}

# Runs as the user, in their home, inside their user manager. stderr is
# folded into the captured output: the stop helper runs after every job and
# must stay quiet when the job already finished, or its output would end up
# in every job's result.
# Unit names must look like real job units (peon-job-<uppercase ID>), or
# the stop helper refuses them.
check T1 "$user" "$(run_as_user peon-job-T1.service 'id -un' 2>&1)"
check T2 "$(getent passwd "$user" | cut -d: -f6)" "$(run_as_user peon-job-T2.service 'pwd' 2>&1)"
check T3 ok "$(run_as_user peon-job-T3.service 'systemctl --user show-environment >/dev/null && echo ok' 2>&1)"
cgroup="$(run_as_user peon-job-T4.service 'cat /proc/self/cgroup' 2>&1)"
check T4 "0::/user.slice/user-$uid.slice/user@$uid.service/app.slice/peon-job-T4.service" "$cgroup"

# Rootless podman gets the systemd cgroup manager, not the cgroupfs fallback.
check T5 systemd "$(run_as_user peon-job-T5.service 'podman info --format "{{.Host.CgroupManager}}"' 2>/dev/null)"

# Exit codes come through both layers, and a failed job is quiet too.
set +e
out="$(run_as_user peon-job-T6.service 'exit 7' 2>&1)"
rc=$?
set -e
check T6-exit 7 "$rc"
check T6-output "" "$out"

# Stopping the outer unit (👎, timeout, shutdown) also stops the inner one.
run_as_user peon-job-T7.service 'sleep 300' 2>&1 | cat > "$tmp/t7.out" &
pid=$!
for _ in $(seq 30); do
    user_systemctl is-active --quiet peon-job-T7.service && break
    sleep 1
done
user_systemctl is-active peon-job-T7.service
systemctl stop peon-job-T7.service
wait "$pid" || true
for _ in $(seq 30); do
    user_systemctl is-active --quiet peon-job-T7.service || break
    sleep 1
done
cat "$tmp/t7.out"
if user_systemctl is-active --quiet peon-job-T7.service; then
    echo "inner unit still running after the outer unit was stopped" >&2
    exit 1
fi
check T7-output "" "$(cat "$tmp/t7.out")"

# The helper refuses anything that isn't a peon job unit.
set +e
out="$(runuser -u "$user" -- env XDG_RUNTIME_DIR="/run/user/$uid" "$helper" dbus-broker.service 2>&1)"
rc=$?
set -e
check refuse-exit 2 "$rc"
grep -q "not a peon job unit" <<< "$out"
