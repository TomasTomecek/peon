package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestArgsPassCommandAsOneArgument(t *testing.T) {
	x := &Systemd{}
	cmd := `echo "$HOME"; rm -rf "/tmp/x y" && false`
	args := x.Args(Spec{JobID: "01JBQ3Y8Z4K7M2N5P6R8S9T0VW", Command: cmd, Timeout: 90 * time.Second}, "0", "/out")
	want := []string{"--quiet", "--unit=peon-job-01JBQ3Y8Z4K7M2N5P6R8S9T0VW.service",
		"--description=peon job 01JBQ3Y8Z4K7M2N5P6R8S9T0VW", "--uid=root", "--working-directory=/",
		"--service-type=exec", "--property=RuntimeMaxSec=90",
		"--property=StandardOutput=append:/out/01JBQ3Y8Z4K7M2N5P6R8S9T0VW.stdout",
		"--property=StandardError=append:/out/01JBQ3Y8Z4K7M2N5P6R8S9T0VW.stderr",
		"--collect", "--wait", "--",
		"/bin/bash", "--noprofile", "--norc", "-c", cmd}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args =\n%q\nwant\n%q", args, want)
	}
}

func TestSplitLines(t *testing.T) {
	var got []string
	SplitLines(strings.NewReader("a\nbb\n\nlast"), func(l string) { got = append(got, l) })
	if strings.Join(got, "|") != "a|bb||last" {
		t.Fatalf("got %q", got)
	}
	got = nil
	SplitLines(strings.NewReader(strings.Repeat("x", MaxLine*2+5)+"\ny"), func(l string) { got = append(got, l) })
	if len(got) != 4 || len(got[0]) != MaxLine || len(got[2]) != 5 || got[3] != "y" {
		t.Fatalf("long line split into %d parts", len(got))
	}
}

// fakeTools stands in for systemd-run and systemctl. The fake systemd-run
// records its PID and execs the job command from its arguments, so the job
// keeps that PID; the fake "systemctl stop" kills it with bash's builtin
// kill. Only bash is needed: build chroots have no pkill.
func fakeTools(t *testing.T) (run, ctl, stopLog string) {
	dir := t.TempDir()
	stopLog = filepath.Join(dir, "stops")
	pidFile := filepath.Join(dir, "pid")
	run = filepath.Join(dir, "systemd-run")
	ctl = filepath.Join(dir, "systemctl")
	os.WriteFile(run, []byte(`#!/bin/bash
echo $$ > `+pidFile+`
out=; err=
for a in "$@"; do
  case "$a" in
    --) break ;;
    --property=StandardOutput=append:*) out=${a#*append:} ;;
    --property=StandardError=append:*) err=${a#*append:} ;;
  esac
done
while [ "$1" != "--" ]; do shift; done; shift
[ -n "$out" ] && exec >"$out"
[ -n "$err" ] && exec 2>"$err"
exec "$@"
`), 0o755)
	os.WriteFile(ctl, []byte(`#!/bin/bash
echo "$@" >> `+stopLog+`
kill -TERM "$(cat `+pidFile+` 2>/dev/null)" 2>/dev/null || true
`), 0o755)
	return run, ctl, stopLog
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	run, ctl, _ := fakeTools(t)
	x := &Systemd{OutputDir: t.TempDir(), poll: 10 * time.Millisecond, SystemdRun: run, Systemctl: ctl}
	var out, errs []string
	res := x.Run(context.Background(), Spec{JobID: "A", Command: "echo one; echo two >&2; echo three; exit 3", Timeout: time.Minute},
		func(l string) { out = append(out, l) }, func(l string) { errs = append(errs, l) })
	if res.Err != nil || res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("result %+v", res)
	}
	if strings.Join(out, ",") != "one,three" || strings.Join(errs, ",") != "two" {
		t.Fatalf("out=%q err=%q", out, errs)
	}
}

func TestCancelStopsUnit(t *testing.T) {
	run, ctl, stopLog := fakeTools(t)
	x := &Systemd{OutputDir: t.TempDir(), poll: 10 * time.Millisecond, SystemdRun: run, Systemctl: ctl}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	res := x.Run(ctx, Spec{JobID: "B", Command: "exec sleep 30", Timeout: time.Minute}, func(string) {}, func(string) {})
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancel did not stop the job")
	}
	if res.ExitCode == 0 {
		t.Fatalf("result %+v", res)
	}
	b, _ := os.ReadFile(stopLog)
	if strings.TrimSpace(string(b)) != "stop peon-job-B.service" {
		t.Fatalf("stop log %q", b)
	}
}

func TestWatchdogMarksTimeout(t *testing.T) {
	run, ctl, _ := fakeTools(t)
	x := &Systemd{OutputDir: t.TempDir(), poll: 10 * time.Millisecond, SystemdRun: run, Systemctl: ctl, Grace: 200 * time.Millisecond}
	res := x.Run(context.Background(), Spec{JobID: "C", Command: "exec sleep 30", Timeout: 300 * time.Millisecond}, func(string) {}, func(string) {})
	if !res.TimedOut {
		t.Fatalf("result %+v", res)
	}
}

func TestMissingBinary(t *testing.T) {
	x := &Systemd{OutputDir: t.TempDir(), SystemdRun: "/nonexistent/systemd-run"}
	res := x.Run(context.Background(), Spec{JobID: "D", Command: "true", Timeout: time.Second}, func(string) {}, func(string) {})
	if res.Err == nil || res.ExitCode != -1 {
		t.Fatalf("result %+v", res)
	}
}

func TestArgsForUserRunInTheirUserManager(t *testing.T) {
	x := &Systemd{}
	args := x.Args(Spec{JobID: "J", Command: "podman ps", Timeout: time.Minute, User: "deploy"}, "1001", "/out")
	want := []string{"--quiet", "--unit=peon-job-J.service", "--description=peon job J (as deploy)",
		"--uid=deploy", "--property=WorkingDirectory=~", "--service-type=exec", "--property=RuntimeMaxSec=60",
		"--setenv=XDG_RUNTIME_DIR=/run/user/1001",
		"--setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus",
		"--property=ExecStopPost=-/usr/libexec/peon/stop-user-unit peon-job-J.service",
		"--property=StandardOutput=append:/out/J.stdout",
		"--property=StandardError=append:/out/J.stderr",
		"--collect", "--wait", "--",
		"/usr/bin/systemd-run", "--user", "--quiet", "--unit=peon-job-J.service", "--description=peon job J",
		"--service-type=exec", "--property=RuntimeMaxSec=60", "--collect", "--wait", "--pipe", "--",
		"/bin/bash", "--noprofile", "--norc", "-c", "podman ps"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args =\n%q\nwant\n%q", args, want)
	}
	if got := x.Args(Spec{JobID: "J", Command: "id", Timeout: time.Minute, User: "root"}, "0", "/out"); got[3] != "--uid=root" || got[len(got)-1] != "id" {
		t.Fatalf("explicit root args %q", got)
	}
}

func TestUserJobNeedsLingering(t *testing.T) {
	run, ctl, _ := fakeTools(t)
	linger := t.TempDir()
	helper := filepath.Join(t.TempDir(), "stop-user-unit")
	x := &Systemd{OutputDir: t.TempDir(), poll: 10 * time.Millisecond, SystemdRun: run, Systemctl: ctl, LingerDir: linger, StopHelper: helper,
		LookupUID: func(name string) (string, error) {
			if name == "ghost" {
				return "", errors.New("unknown user")
			}
			return "1001", nil
		}}
	res := x.Run(context.Background(), Spec{JobID: "U", Command: "true", Timeout: time.Second, User: "deploy"}, func(string) {}, func(string) {})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "loginctl enable-linger deploy") {
		t.Fatalf("result %+v", res)
	}
	res = x.Run(context.Background(), Spec{JobID: "U", Command: "true", Timeout: time.Second, User: "ghost"}, func(string) {}, func(string) {})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "unknown user") {
		t.Fatalf("result %+v", res)
	}
	os.WriteFile(filepath.Join(linger, "deploy"), nil, 0o644)
	res = x.Run(context.Background(), Spec{JobID: "U", Command: "true", Timeout: time.Second, User: "deploy"}, func(string) {}, func(string) {})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "missing or not executable") {
		t.Fatalf("missing helper not detected: %+v", res)
	}
	os.WriteFile(helper, []byte("#!/bin/bash\n"), 0o755)
	// The fake systemd-run runs whatever follows the first "--": here the
	// inner systemd-run, which the fake PATH does not have. Seeing it be
	// invoked is enough to know the checks passed.
	res = x.Run(context.Background(), Spec{JobID: "U", Command: "true", Timeout: time.Second, User: "deploy"}, func(string) {}, func(string) {})
	if res.Err != nil {
		t.Fatalf("checks did not pass: %+v", res)
	}
}

func TestOutputStreamsWhileJobRuns(t *testing.T) {
	run, ctl, _ := fakeTools(t)
	x := &Systemd{OutputDir: t.TempDir(), poll: 20 * time.Millisecond, SystemdRun: run, Systemctl: ctl}
	early := make(chan struct{}, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		x.Run(context.Background(), Spec{JobID: "S", Command: "echo early; sleep 2; echo late", Timeout: time.Minute},
			func(l string) {
				if l == "early" {
					early <- struct{}{}
				}
			}, func(string) {})
	}()
	select {
	case <-early:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("early output was not streamed while the job ran")
	}
	select {
	case <-finished:
		t.Fatal("job ended before the early line was checked")
	default:
	}
	<-finished
}

func TestOutputFilesRemovedAndSystemdRunErrorsCaptured(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(t.TempDir(), "systemd-run")
	os.WriteFile(fake, []byte("#!/bin/bash\necho 'Failed to start transient service unit: X' >&2\nexit 1\n"), 0o755)
	x := &Systemd{OutputDir: filepath.Join(dir, "out"), poll: 10 * time.Millisecond, SystemdRun: fake}
	// A leftover from an earlier run must not leak into this job.
	os.MkdirAll(x.OutputDir, 0o700)
	os.WriteFile(filepath.Join(x.OutputDir, "E.stdout"), []byte("stale\n"), 0o600)
	var out, errs []string
	res := x.Run(context.Background(), Spec{JobID: "E", Command: "true", Timeout: time.Minute},
		func(l string) { out = append(out, l) }, func(l string) { errs = append(errs, l) })
	if res.Err != nil || res.ExitCode != 1 {
		t.Fatalf("result %+v", res)
	}
	if len(out) != 0 || strings.Join(errs, "|") != "Failed to start transient service unit: X" {
		t.Fatalf("out=%q err=%q", out, errs)
	}
	ents, _ := os.ReadDir(x.OutputDir)
	if len(ents) != 0 {
		t.Fatalf("output files left behind: %v", ents)
	}
	if fi, err := os.Stat(x.OutputDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("output dir %v %v", fi, err)
	}
}

func TestFollowReaderFinalReadAndLateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	done := make(chan struct{})
	r := &followReader{path: path, done: done, poll: 10 * time.Millisecond}
	defer r.Close()
	var mu sync.Mutex
	var got []string
	finished := make(chan struct{})
	go func() {
		SplitLines(r, func(l string) { mu.Lock(); got = append(got, l); mu.Unlock() })
		close(finished)
	}()
	time.Sleep(50 * time.Millisecond) // file does not exist yet
	os.WriteFile(path, []byte("a\nb"), 0o600)
	time.Sleep(50 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("c\nlast")
	f.Close()
	close(done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("follower did not finish after done")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "|") != "a|bc|last" {
		t.Fatalf("got %q", got)
	}

	// A file that never appears ends with EOF.
	done2 := make(chan struct{})
	close(done2)
	r2 := &followReader{path: filepath.Join(t.TempDir(), "none"), done: done2}
	if n, err := r2.Read(make([]byte, 8)); n != 0 || err == nil {
		t.Fatalf("read %d %v", n, err)
	}
}
