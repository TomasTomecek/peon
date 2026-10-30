// Package executor runs approved commands in transient systemd units
// (SPEC.md §7.1).
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Spec struct {
	JobID   string
	Command string
	Timeout time.Duration
	// User runs the job; empty means root.
	User string
}

type Result struct {
	// ExitCode is the command's exit status, or -1 if it is unknown.
	ExitCode int
	// TimedOut is set when the command ran out of time.
	TimedOut bool
	// Err is set when the job could not be run or waited for at all.
	Err error
}

type Executor interface {
	// Run blocks until the job finishes. Cancelling ctx stops the job.
	// stdout and stderr receive complete lines, without the newline.
	Run(ctx context.Context, s Spec, stdout, stderr func(string)) Result
	// Stop stops a job unit, whether or not this process started it.
	Stop(ctx context.Context, jobID string) error
}

// StopUserUnit is the ExecStopPost= helper installed by the RPM
// (dist/stop-user-unit). See that script for why it is not systemctl.
const StopUserUnit = "/usr/libexec/peon/stop-user-unit"

// UnitName is the systemd unit a job runs in.
func UnitName(jobID string) string { return "peon-job-" + jobID + ".service" }

type Systemd struct {
	SystemdRun string // default "systemd-run"
	Systemctl  string // default "systemctl"
	// LookupUID resolves a user name to a UID. Default: os/user.
	LookupUID func(name string) (string, error)
	// LingerDir is where logind marks lingering users. Default:
	// /var/lib/systemd/linger.
	LingerDir string
	// StopHelper overrides StopUserUnit, for tests.
	StopHelper string
	// Grace is how long past the timeout peon waits before stopping the
	// unit itself, in case RuntimeMaxSec did not fire.
	Grace time.Duration
	// OutputDir holds the files PID 1 writes a job's stdout and stderr
	// to while it runs. Default: /var/lib/peon/output. It must be
	// root-only; files are removed when the job ends.
	OutputDir string

	// poll is how often followReader checks a quiet file. Default 200ms.
	poll time.Duration
}

// DefaultOutputDir is used when Systemd.OutputDir is empty.
const DefaultOutputDir = "/var/lib/peon/output"

func (x *Systemd) outDir() string {
	if x.OutputDir == "" {
		return DefaultOutputDir
	}
	return x.OutputDir
}

// OutputPaths returns the files a job's stdout and stderr go to.
func OutputPaths(dir, jobID string) (stdout, stderr string) {
	return filepath.Join(dir, jobID+".stdout"), filepath.Join(dir, jobID+".stderr")
}

func (x *Systemd) bin(p, def string) string {
	if p == "" {
		return def
	}
	return p
}

// Args returns the systemd-run arguments for a job. The command is passed
// as a single argv element to bash; nothing is interpolated into a shell
// by peon.
//
// Root jobs run in a system unit. Jobs for another user run in that
// user's own systemd user manager, so they get the user's real session
// environment (XDG_RUNTIME_DIR, the user bus, cgroup delegation), which
// rootless podman and systemctl --user need. To get there without extra
// privileges, a system unit running as the user starts the command with
// "systemd-run --user"; stopping the system unit (cancel, timeout) stops
// the inner unit through its ExecStopPost= helper.
//
// The system unit does not get --pipe. --pipe makes systemd-run hand its
// own stdin, stdout and stderr to PID 1 over D-Bus, and under SELinux
// PID 1 (init_t) may not read or write pipes created by a service
// process, so PID 1 drops the connection ("Connection reset by peer").
// Instead PID 1 opens the output files itself (StandardOutput=truncate:),
// so no file descriptor crosses the bus, and peon follows the files. For
// user jobs the inner "systemd-run --user --pipe" passes the outer unit's
// descriptors, which PID 1 opened, not peon's pipes.
//
// The output paths contain the job ID, which peon generates (a ULID) and
// is never user input, so putting it in a property does not break the "no
// input in unit properties" rule.
func (x *Systemd) Args(s Spec, uid, outDir string) []string {
	secs := int64(s.Timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	runtime := "--property=RuntimeMaxSec=" + strconv.FormatInt(secs, 10)
	unit := UnitName(s.JobID)
	shell := []string{"/bin/bash", "--noprofile", "--norc", "-c", s.Command}
	outPath, errPath := OutputPaths(outDir, s.JobID)
	outProps := []string{
		"--property=StandardOutput=truncate:" + outPath,
		"--property=StandardError=truncate:" + errPath,
	}
	if s.User == "" || s.User == "root" {
		return append([]string{
			"--quiet",
			"--unit=" + unit,
			"--description=peon job " + s.JobID,
			"--uid=root",
			"--working-directory=/",
			"--service-type=exec",
			runtime,
			outProps[0], outProps[1],
			"--collect", "--wait",
			"--",
		}, shell...)
	}
	rundir := "/run/user/" + uid
	outer := []string{
		"--quiet",
		"--unit=" + unit,
		"--description=peon job " + s.JobID + " (as " + s.User + ")",
		"--uid=" + s.User,
		"--property=WorkingDirectory=~",
		"--service-type=exec",
		runtime,
		"--setenv=XDG_RUNTIME_DIR=" + rundir,
		"--setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=" + rundir + "/bus",
		"--property=ExecStopPost=-" + x.bin(x.StopHelper, StopUserUnit) + " " + unit,
		outProps[0], outProps[1],
		"--collect", "--wait",
		"--",
	}
	inner := []string{
		"/usr/bin/systemd-run", "--user",
		"--quiet",
		"--unit=" + unit,
		"--description=peon job " + s.JobID,
		"--service-type=exec",
		runtime,
		"--collect", "--wait", "--pipe",
		"--",
	}
	return append(append(outer, inner...), shell...)
}

// prepare resolves the user's UID and checks that their user manager
// runs without a login (lingering).
func (x *Systemd) prepare(s Spec) (string, error) {
	if s.User == "" || s.User == "root" {
		return "0", nil
	}
	lookup := x.LookupUID
	if lookup == nil {
		lookup = func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.Uid, nil
		}
	}
	uid, err := lookup(s.User)
	if err != nil {
		return "", fmt.Errorf("user %q: %w", s.User, err)
	}
	dir := x.LingerDir
	if dir == "" {
		dir = "/var/lib/systemd/linger"
	}
	if _, err := os.Stat(filepath.Join(dir, s.User)); err != nil {
		return "", fmt.Errorf("user %q has no lingering systemd user manager (loginctl enable-linger %s)", s.User, s.User)
	}
	// Without the helper the job would run but could never be stopped.
	helper := x.bin(x.StopHelper, StopUserUnit)
	if fi, err := os.Stat(helper); err != nil || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is missing or not executable; reinstall peon", helper)
	}
	return uid, nil
}

func (x *Systemd) Run(ctx context.Context, s Spec, stdout, stderr func(string)) Result {
	uid, err := x.prepare(s)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	dir := x.outDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("output dir: %w", err)}
	}
	outPath, errPath := OutputPaths(dir, s.JobID)
	cleanup := func() { os.Remove(outPath); os.Remove(errPath) }
	cleanup()
	defer cleanup()

	// Several goroutines feed each callback; serialize them.
	var outMu, errMu sync.Mutex
	stdoutFn := func(l string) { outMu.Lock(); defer outMu.Unlock(); stdout(l) }
	stderrFn := func(l string) { errMu.Lock(); defer errMu.Unlock(); stderr(l) }

	cmd := exec.Command(x.bin(x.SystemdRun, "systemd-run"), x.Args(s, uid, dir)...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	// These pipes carry systemd-run's own messages only. Without --pipe
	// they are not passed to PID 1.
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("start systemd-run: %w", err)}
	}

	// Closed once systemd-run has returned, i.e. the unit has exited.
	unitDone := make(chan struct{})
	poll := x.poll
	var pipeReaders, fileReaders sync.WaitGroup
	pipeReaders.Add(2)
	go func() { defer pipeReaders.Done(); SplitLines(outPipe, stdoutFn) }()
	go func() { defer pipeReaders.Done(); SplitLines(errPipe, stderrFn) }()
	fileReaders.Add(2)
	go func() {
		defer fileReaders.Done()
		fr := &followReader{path: outPath, done: unitDone, poll: poll}
		defer fr.Close()
		SplitLines(fr, stdoutFn)
	}()
	go func() {
		defer fileReaders.Done()
		fr := &followReader{path: errPath, done: unitDone, poll: poll}
		defer fr.Close()
		SplitLines(fr, stderrFn)
	}()

	// Stop the unit if we are cancelled, or if it outlives its timeout.
	grace := x.Grace
	if grace == 0 {
		grace = 30 * time.Second
	}
	watchdog := time.NewTimer(s.Timeout + grace)
	defer watchdog.Stop()
	done := make(chan struct{})
	var stopped sync.WaitGroup
	stopped.Add(1)
	go func() {
		defer stopped.Done()
		select {
		case <-done:
			return
		case <-ctx.Done():
		case <-watchdog.C:
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = x.Stop(stopCtx, s.JobID)
	}()

	pipeReaders.Wait()
	waitErr := cmd.Wait()
	close(unitDone)
	fileReaders.Wait()
	close(done)
	stopped.Wait()
	elapsed := time.Since(start)

	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			return Result{ExitCode: -1, Err: waitErr}
		}
		code = ee.ExitCode()
	}
	return Result{
		ExitCode: code,
		// systemd-run does not report why the unit ended; a non-zero exit
		// at or past the timeout means RuntimeMaxSec killed it.
		TimedOut: code != 0 && elapsed >= s.Timeout,
	}
}

func (x *Systemd) Stop(ctx context.Context, jobID string) error {
	cmd := exec.CommandContext(ctx, x.bin(x.Systemctl, "systemctl"), "stop", UnitName(jobID))
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl stop %s: %w: %s", UnitName(jobID), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// followReader reads a file that another process is still writing, and
// that may not exist yet. At EOF it waits poll (or until done is closed)
// and retries. After done is closed it reads to EOF one last time, then
// returns io.EOF.
type followReader struct {
	path string
	done <-chan struct{}
	poll time.Duration // default 200ms
	f    *os.File
}

func (r *followReader) isDone() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *followReader) Read(p []byte) (int, error) {
	poll := r.poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	for {
		// Sample before reading so that a read to EOF after done means
		// nothing more can arrive.
		finished := r.isDone()
		if r.f == nil {
			f, err := os.OpenFile(r.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err == nil {
				r.f = f
			}
		}
		if r.f != nil {
			n, err := r.f.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil && err != io.EOF {
				return 0, err
			}
		}
		if finished {
			return 0, io.EOF
		}
		t := time.NewTimer(poll)
		select {
		case <-t.C:
		case <-r.done:
			t.Stop()
		}
	}
}

func (r *followReader) Close() {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}

// MaxLine is the longest line SplitLines emits; longer lines are split.
const MaxLine = 64 << 10

// SplitLines reads r to EOF and calls fn for each line. It never stops
// reading early, so a job can never block on a full pipe.
func SplitLines(r io.Reader, fn func(string)) {
	buf := make([]byte, 32<<10)
	var pending []byte
	for {
		n, err := r.Read(buf)
		data := buf[:n]
		for len(data) > 0 {
			i := indexByte(data, '\n')
			if i < 0 {
				pending = append(pending, data...)
				break
			}
			pending = append(pending, data[:i]...)
			fn(string(pending))
			pending = pending[:0]
			data = data[i+1:]
		}
		for len(pending) >= MaxLine {
			fn(string(pending[:MaxLine]))
			pending = append(pending[:0], pending[MaxLine:]...)
		}
		if err != nil {
			if len(pending) > 0 {
				fn(string(pending))
			}
			return
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
