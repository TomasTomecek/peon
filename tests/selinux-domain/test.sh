#!/bin/bash
# Keep run_as_root in sync with internal/executor Systemd.Args.
#
# The job's shell is started by PID 1, so the domain it runs in depends on
# the unit's properties and the policy, not on who calls systemd-run. The
# output files live under /var/lib like peon's (var_lib_t), because the
# lost output comes from confined domains that may not write var_lib_t.

exec 9>&2
BASH_XTRACEFD=9
set -euxo pipefail

# The test only means something with SELinux enforcing: permissive mode
# logs the denials but lets the output through.
if [ "$(getenforce)" != Enforcing ]; then
    echo "SELinux is $(getenforce), this test needs Enforcing" >&2
    exit 1
fi

diagnose() {
    set +e
    if command -v ausearch >/dev/null; then
        ausearch -m AVC,USER_AVC,SELINUX_ERR -ts recent
    fi
    journalctl --no-pager -n 300 | grep -F peon-job | tail -n 50
}
outdir=/var/lib/peon-selinux-test-output
rm -rf "$outdir"
mkdir -m 700 "$outdir"
trap 'rc=$?; if [ "$rc" -ne 0 ]; then diagnose; fi; rm -rf "$outdir"' EXIT

# run_as_root UNIT COMMAND: prints the job's stdout, then its stderr.
run_as_root() {
    local unit="$1" cmd="$2" rc=0
    local out="$outdir/$unit.stdout" err="$outdir/$unit.stderr"
    systemd-run --quiet \
        --unit="$unit" --description="peon job test" \
        --uid=root --working-directory=/ --service-type=exec \
        --expand-environment=no --property=RuntimeMaxSec=60 \
        --property=SELinuxContext=system_u:system_r:unconfined_service_t:s0 \
        "--property=StandardOutput=append:$out" \
        "--property=StandardError=append:$err" \
        --collect --wait -- \
        /bin/bash --noprofile --norc -c "$cmd" || rc=$?
    cat "$out" "$err" 2>/dev/null || true
    rm -f "$out" "$err"
    return "$rc"
}

check() {
    if [ "$3" != "$2" ]; then
        printf '%s: want %q\n%s:  got %q\n' "$1" "$2" "$1" "$3" >&2
        exit 1
    fi
}

# ip (ifconfig_t) and ping (ping_t) have their own domains, which initrc_t
# would transition into; their output must reach the output file. Each
# command runs in the same multi-line job, like the report in tt/peon#3.
out="$(run_as_root peon-job-S1.service 'echo first
ip -o link show lo
ping -c1 -W5 127.0.0.1
echo last')"
printf '%s\n' "$out"
grep -q '^first$' <<< "$out"
grep -q ': lo: ' <<< "$out"
grep -q '1 packets transmitted, 1 received' <<< "$out"
grep -q '^last$' <<< "$out"

# stderr of a confined program comes through too.
out="$(run_as_root peon-job-S2.service 'ip link show peonnodev0' 2>&1)" || true
grep -q 'peonnodev0' <<< "$out"

# Shell variables reach bash unexpanded: systemd-run would otherwise
# replace "${d}" with the unit's (empty) value before bash runs.
# shellcheck disable=SC2016 # bash, not this script, expands it
check vars /srv "$(run_as_root peon-job-S4.service 'd=/srv
cd "${d}" && pwd')"

# The shell runs in unconfined_service_t, not initrc_t.
check domain system_u:system_r:unconfined_service_t:s0 "$(run_as_root peon-job-S3.service 'id -Z')"
