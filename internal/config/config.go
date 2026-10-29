// Package config loads and validates /etc/peon/config.toml (SPEC.md §10).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where the RPM installs the configuration file.
const DefaultPath = "/etc/peon/config.toml"

// Duration wraps time.Duration so it can be written as "30m" in TOML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Config struct {
	Host    string  `toml:"host"`
	Forgejo Forgejo `toml:"forgejo"`
	Limits  Limits  `toml:"limits"`
	Loki    Loki    `toml:"loki"`
	Grafana Grafana `toml:"grafana"`
}

type Forgejo struct {
	URL          string   `toml:"url"`
	Repos        []string `toml:"repos"`
	BotUserID    int64    `toml:"bot_user_id"`
	BotTokenFile string   `toml:"bot_token_file"`
	ApproverID   int64    `toml:"approver_id"`
	RequesterIDs []int64  `toml:"requester_ids"`
}

type Limits struct {
	JobTTL          Duration `toml:"job_ttl"`
	DefaultTimeout  Duration `toml:"default_timeout"`
	MaxTimeout      Duration `toml:"max_timeout"`
	MaxCommandBytes int      `toml:"max_command_bytes"`
	OutputTailLines int      `toml:"output_tail_lines"`
	PollInterval    Duration `toml:"poll_interval"`
}

type Loki struct {
	URL          string `toml:"url"`
	User         string `toml:"user"`
	PasswordFile string `toml:"password_file"`
}

type Grafana struct {
	ExploreURL string `toml:"explore_url"`
}

var (
	hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func defaults() Config {
	return Config{Limits: Limits{
		JobTTL:          Duration{30 * time.Minute},
		DefaultTimeout:  Duration{10 * time.Minute},
		MaxTimeout:      Duration{2 * time.Hour},
		MaxCommandBytes: 4096,
		OutputTailLines: 50,
		PollInterval:    Duration{10 * time.Second},
	}}
}

// Load parses the file at path, applies defaults and validates the result.
// Unknown keys are an error: a typo in a security setting must not be ignored.
func Load(path string) (*Config, error) {
	c := defaults()
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks every setting and returns all problems at once.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !hostRe.MatchString(c.Host) {
		bad("host %q: must be a lowercase DNS label", c.Host)
	}

	f := c.Forgejo
	checkURL("forgejo.url", f.URL, bad)
	if len(f.Repos) == 0 {
		bad("forgejo.repos: at least one repo is required")
	}
	for _, r := range f.Repos {
		if !repoRe.MatchString(r) {
			bad("forgejo.repos: %q is not owner/name", r)
		}
	}
	if f.BotUserID <= 0 {
		bad("forgejo.bot_user_id: must be a positive user ID")
	}
	if f.ApproverID <= 0 {
		bad("forgejo.approver_id: must be a positive user ID")
	}
	if f.ApproverID > 0 && f.ApproverID == f.BotUserID {
		bad("forgejo.approver_id must differ from bot_user_id")
	}
	if len(f.RequesterIDs) == 0 {
		bad("forgejo.requester_ids: at least one requester is required")
	}
	for _, id := range f.RequesterIDs {
		if id <= 0 {
			bad("forgejo.requester_ids: %d is not a valid user ID", id)
		}
		if id == f.BotUserID {
			bad("forgejo.requester_ids must not contain bot_user_id")
		}
	}
	checkSecretFile("forgejo.bot_token_file", f.BotTokenFile, bad)

	l := c.Limits
	if l.JobTTL.Duration <= 0 {
		bad("limits.job_ttl: must be positive")
	}
	if l.DefaultTimeout.Duration <= 0 || l.MaxTimeout.Duration <= 0 {
		bad("limits.default_timeout and limits.max_timeout: must be positive")
	}
	if l.DefaultTimeout.Duration > l.MaxTimeout.Duration {
		bad("limits.default_timeout (%s) exceeds limits.max_timeout (%s)", l.DefaultTimeout, l.MaxTimeout)
	}
	if l.MaxCommandBytes <= 0 || l.MaxCommandBytes > 65536 {
		bad("limits.max_command_bytes: must be between 1 and 65536")
	}
	if l.OutputTailLines < 0 {
		bad("limits.output_tail_lines: must not be negative")
	}
	if l.PollInterval.Duration < time.Second {
		bad("limits.poll_interval: must be at least 1s")
	}

	checkURL("loki.url", c.Loki.URL, bad)
	if c.Loki.User == "" {
		bad("loki.user: required")
	}
	checkSecretFile("loki.password_file", c.Loki.PasswordFile, bad)

	checkURL("grafana.explore_url", c.Grafana.ExploreURL, bad)

	return errors.Join(errs...)
}

func checkURL(name, raw string, bad func(string, ...any)) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		bad("%s %q: must be an http(s) URL", name, raw)
	}
}

// checkSecretFile requires the file to exist, be a regular file, be non-empty
// and not be readable by group or others.
func checkSecretFile(name, path string, bad func(string, ...any)) {
	if path == "" {
		bad("%s: required", name)
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		bad("%s: %v", name, err)
		return
	}
	if !st.Mode().IsRegular() {
		bad("%s %s: not a regular file", name, path)
		return
	}
	if st.Mode().Perm()&0o077 != 0 {
		bad("%s %s: mode %04o is too open, use 0600", name, path, st.Mode().Perm())
	}
	if st.Size() == 0 {
		bad("%s %s: file is empty", name, path)
	}
}

// ReadSecret returns the trimmed contents of a secret file.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
