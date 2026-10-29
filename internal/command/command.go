// Package command parses "/run" comments and validates the command they
// carry (SPEC.md §4 and §5.3).
package command

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrNotRun means the comment is not a /run request at all. Callers ignore
// such comments silently.
var ErrNotRun = errors.New("not a /run comment")

// Request is a parsed /run comment.
type Request struct {
	Host string
	// User is empty when the requester did not set --user.
	User    string
	Private bool
	// Timeout is zero when the requester did not set --timeout.
	Timeout time.Duration
	Command string
}

var (
	hostRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	userRe  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	fenceRe = regexp.MustCompile("^(`{3,})([A-Za-z]*)[ \t]*$")
)

// Parse parses a comment body. If the first line names a host, req.Host is
// set even when an error is returned, so the caller can decide whether the
// error concerns this host before replying.
func Parse(body string) (req Request, err error) {
	// Web UIs submit CRLF line endings. Lone CRs survive and are rejected
	// later by Validate.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	header := strings.Fields(lines[0])
	if len(header) == 0 || header[0] != "/run" {
		return req, ErrNotRun
	}
	if len(header) < 2 {
		return req, errors.New("missing host: the first line must be \"/run <host>\"")
	}
	if !hostRe.MatchString(header[1]) {
		return req, fmt.Errorf("invalid host %q", header[1])
	}
	req.Host = header[1]

	for _, f := range header[2:] {
		switch {
		case f == "--private":
			if req.Private {
				return req, errors.New("--private given twice")
			}
			req.Private = true
		case strings.HasPrefix(f, "--user="):
			if req.User != "" {
				return req, errors.New("--user given twice")
			}
			u := strings.TrimPrefix(f, "--user=")
			if !userRe.MatchString(u) {
				return req, fmt.Errorf("invalid %s: not a valid user name", f)
			}
			req.User = u
		case strings.HasPrefix(f, "--timeout="):
			if req.Timeout != 0 {
				return req, errors.New("--timeout given twice")
			}
			d, perr := time.ParseDuration(strings.TrimPrefix(f, "--timeout="))
			if perr != nil || d <= 0 {
				return req, fmt.Errorf("invalid %s: want a positive duration such as 10m", f)
			}
			req.Timeout = d
		default:
			return req, fmt.Errorf("unknown flag %q (allowed: --user=<name>, --private, --timeout=<duration>)", f)
		}
	}

	// Skip blank lines up to the opening fence.
	i := 1
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) {
		return req, errors.New("missing command: put it in a fenced code block after the /run line")
	}
	m := fenceRe.FindStringSubmatch(lines[i])
	if m == nil {
		return req, errors.New("expected a fenced code block (```) after the /run line")
	}
	fence, lang := m[1], m[2]
	if lang != "" && lang != "sh" && lang != "bash" {
		return req, fmt.Errorf("unsupported code block language %q (use sh, bash or none)", lang)
	}

	var cmd []string
	closed := false
	for i++; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == fence {
			closed = true
			break
		}
		cmd = append(cmd, lines[i])
	}
	if !closed {
		return req, errors.New("code block is not closed")
	}
	for i++; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return req, errors.New("unexpected text after the code block: a /run comment holds exactly one command block and nothing else")
		}
	}
	req.Command = strings.Join(cmd, "\n")
	return req, nil
}

type warnPattern struct {
	re  *regexp.Regexp
	msg string
}

var warnPatterns = []warnPattern{
	{regexp.MustCompile(`base64`), "uses base64 (encoded payloads hide what runs)"},
	{regexp.MustCompile(`(curl|wget)\b[^|\n]*\|\s*(ba|z|da)?sh\b`), "pipes a download into a shell"},
	{regexp.MustCompile(`\beval\b`), "uses eval"},
	{regexp.MustCompile(`\bxxd\b`), "uses xxd"},
	{regexp.MustCompile(`\\x[0-9A-Fa-f]{2}`), "contains \\x escapes"},
	{regexp.MustCompile(`\brm\s+(-[A-Za-z]*\s+)*-[A-Za-z]*[rR][A-Za-z]*\s+(-[A-Za-z]*\s+)*/(\s|$|\*)`), "removes files recursively from /"},
	{regexp.MustCompile(`/dev/(sd|nvme|vd|xvd|hd|mmcblk)`), "touches a raw block device"},
	{regexp.MustCompile(`\bmkfs`), "creates a filesystem"},
	{regexp.MustCompile(`\bdd\b`), "uses dd"},
}

// Validate applies the hard rejects and returns warnings to display on the
// job card. Warnings never block a job; the operator decides.
func Validate(cmd string, maxBytes int) (warnings []string, err error) {
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("the command is empty")
	}
	if len(cmd) > maxBytes {
		return nil, fmt.Errorf("the command is %d bytes, over the limit of %d", len(cmd), maxBytes)
	}
	line, col := 1, 0
	for i := 0; i < len(cmd); i++ {
		b := cmd[i]
		col++
		switch {
		case b == '\n':
			line, col = line+1, 0
		case b == '\t' || (b >= 0x20 && b <= 0x7e):
		default:
			return nil, fmt.Errorf("byte 0x%02x at line %d, column %d is not printable ASCII; non-ASCII, control characters and CR are rejected so the card shows exactly what runs", b, line, col)
		}
	}

	lines := strings.Split(cmd, "\n")
	if len(lines) > 1 {
		warnings = append(warnings, fmt.Sprintf("%d lines: read every line", len(lines)))
	}
	for n, l := range lines {
		if len(l) > 80 {
			warnings = append(warnings, fmt.Sprintf("line %d is %d characters long (hard to read on a phone)", n+1, len(l)))
		}
	}
	for _, p := range warnPatterns {
		if p.re.MatchString(cmd) {
			warnings = append(warnings, p.msg)
		}
	}
	return warnings, nil
}
