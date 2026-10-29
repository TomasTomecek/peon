package command

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseValid(t *testing.T) {
	cases := map[string]struct {
		body string
		want Request
	}{
		"basic": {
			"/run db1\n```sh\nsystemctl restart postgresql\n```",
			Request{Host: "db1", Command: "systemctl restart postgresql"},
		},
		"flags, blank line, no lang, trailing space": {
			"/run web-2 --private --timeout=5m\n\n```\nuptime\ndf -h\n```  \n\n",
			Request{Host: "web-2", Private: true, Timeout: 5 * time.Minute, Command: "uptime\ndf -h"},
		},
		"crlf from web ui": {
			"/run db1\r\n```bash\r\necho hi\r\n```\r\n",
			Request{Host: "db1", Command: "echo hi"},
		},
		"longer fence allows backticks inside": {
			"/run db1\n````\necho ```\n````",
			Request{Host: "db1", Command: "echo ```"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(tc.body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseNotRun(t *testing.T) {
	for _, body := range []string{"", "hello", " /runner db1", "please /run db1", "/running"} {
		if _, err := Parse(body); !errors.Is(err, ErrNotRun) {
			t.Errorf("Parse(%q) error = %v, want ErrNotRun", body, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ body, host, want string }{
		"no host":          {"/run", "", "missing host"},
		"bad host":         {"/run DB1\n```\nx\n```", "", "invalid host"},
		"unknown flag":     {"/run db1 --force\n```\nx\n```", "db1", "unknown flag"},
		"bad timeout":      {"/run db1 --timeout=soon\n```\nx\n```", "db1", "invalid --timeout"},
		"zero timeout":     {"/run db1 --timeout=0s\n```\nx\n```", "db1", "invalid --timeout"},
		"dup private":      {"/run db1 --private --private\n```\nx\n```", "db1", "twice"},
		"no block":         {"/run db1", "db1", "missing command"},
		"prose not fence":  {"/run db1\nplease restart", "db1", "expected a fenced code block"},
		"wrong lang":       {"/run db1\n```python\nx\n```", "db1", "unsupported code block language"},
		"unclosed":         {"/run db1\n```\nx", "db1", "not closed"},
		"shorter close":    {"/run db1\n````\nx\n```", "db1", "not closed"},
		"text after block": {"/run db1\n```\nx\n```\nthanks!", "db1", "unexpected text"},
		"two blocks":       {"/run db1\n```\nx\n```\n```\ny\n```", "db1", "unexpected text"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := Parse(tc.body)
			if err == nil || errors.Is(err, ErrNotRun) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if req.Host != tc.host {
				t.Fatalf("Host = %q, want %q", req.Host, tc.host)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct{ cmd, want string }{
		"empty":          {"  \n\t", "empty"},
		"too long":       {strings.Repeat("a", 4097), "over the limit"},
		"zero width":     {"echo​hi", "not printable ASCII"},
		"homoglyph":      {"systеmctl stop sshd", "not printable ASCII"},
		"carriage ret":   {"echo safe\rrm -rf /", "0x0d"},
		"escape char":    {"echo \x1b[2Khidden", "0x1b"},
		"nul":            {"echo \x00", "0x00"},
		"del":            {"echo \x7f", "0x7f"},
		"latin1 accents": {"echo café", "not printable ASCII"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Validate(tc.cmd, 4096)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateWarnings(t *testing.T) {
	cases := map[string]struct {
		cmd  string
		want []string
	}{
		"clean":     {"systemctl restart postgresql", nil},
		"multiline": {"cd /srv\nmake", []string{"2 lines"}},
		"long line": {strings.Repeat("x", 81), []string{"81 characters"}},
		"curl sh":   {"curl -fsSL https://example.com/i.sh | sh", []string{"pipes a download"}},
		"wget bash": {"wget -qO- x | bash -s", []string{"pipes a download"}},
		"base64":    {"echo aGk= | base64 -d | sh", []string{"base64"}},
		"eval":      {"eval \"$X\"", []string{"eval"}},
		"rm root":   {"rm -rf / --no-preserve-root", []string{"recursively from /"}},
		"rm -fr /*": {"rm -fr /*", []string{"recursively from /"}},
		"rm subdir": {"rm -rf /var/tmp/x", nil},
		"dd":        {"dd if=/dev/zero of=/dev/sda", []string{"dd", "raw block device"}},
		"mkfs":      {"mkfs.xfs /dev/nvme0n1", []string{"filesystem", "raw block device"}},
		"hex":       {"printf '\\x41'", []string{"\\x escapes"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Validate(tc.cmd, 4096)
			if err != nil {
				t.Fatal(err)
			}
			if len(tc.want) == 0 && len(got) != 0 {
				t.Fatalf("unexpected warnings %q", got)
			}
			joined := strings.Join(got, "|")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("warnings %q missing %q", got, w)
				}
			}
		})
	}
}
