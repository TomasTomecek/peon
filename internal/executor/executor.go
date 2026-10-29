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
	// Grace is how long past the timeout peon waits before stopping the
	// unit itself, in case RuntimeMaxSec did not fire.
	Grace time.Duration
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
// the inner unit through ExecStopPost.
func (x *Systemd) Args(s Spec, uid string) []string {
	secs := int64(s.Timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	runtime := "--property=RuntimeMaxSec=" + strconv.FormatInt(secs, 10)
	unit := UnitName(s.JobID)
	shell := []string{"/bin/bash", "--noprofile", "--norc", "-c", s.Command}
	if s.User == "" || s.User == "root" {
		return append([]string{
			"--quiet",
			"--unit=" + unit,
			"--description=peon job " + s.JobID,
			"--uid=root",
			"--working-directory=/",
			"--service-type=exec",
			runtime,
			"--collect", "--wait", "--pipe",
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
		"--property=ExecStopPost=-/usr/bin/systemctl --user stop " + unit,
		"--collect", "--wait", "--pipe",
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
	return uid, nil
}

func (x *Systemd) Run(ctx context.Context, s Spec, stdout, stderr func(string)) Result {
	uid, err := x.prepare(s)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	cmd := exec.Command(x.bin(x.SystemdRun, "systemd-run"), x.Args(s, uid)...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
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

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); SplitLines(outPipe, stdout) }()
	go func() { defer readers.Done(); SplitLines(errPipe, stderr) }()

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

	readers.Wait()
	waitErr := cmd.Wait()
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
