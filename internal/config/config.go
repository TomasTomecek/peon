// Package config loads and validates /etc/peon/config.yaml (SPEC.md §10).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where the RPM installs the configuration file.
const DefaultPath = "/etc/peon/config.yaml"

// Duration wraps time.Duration so it can be written as "30m" in YAML.
// Only duration strings are accepted: a bare number like 30 is ambiguous
// (seconds? nanoseconds?) and is rejected.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("line %d: %q is not a duration string such as \"30m\"", n.Line, n.Value)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

type Config struct {
	Host    string  `yaml:"host"`
	Forgejo Forgejo `yaml:"forgejo"`
	Limits  Limits  `yaml:"limits"`
	Jobs    Jobs    `yaml:"jobs"`
	Loki    Loki    `yaml:"loki"`
	Grafana Grafana `yaml:"grafana"`
}

type Forgejo struct {
	URL          string   `yaml:"url"`
	Repos        []string `yaml:"repos"`
	BotUserID    int64    `yaml:"bot_user_id"`
	BotTokenFile string   `yaml:"bot_token_file"`
	ApproverID   int64    `yaml:"approver_id"`
	RequesterIDs []int64  `yaml:"requester_ids"`
}

type Limits struct {
	JobTTL          Duration `yaml:"job_ttl"`
	DefaultTimeout  Duration `yaml:"default_timeout"`
	MaxTimeout      Duration `yaml:"max_timeout"`
	MaxCommandBytes int      `yaml:"max_command_bytes"`
	OutputTailLines int      `yaml:"output_tail_lines"`
	PollInterval    Duration `yaml:"poll_interval"`
}

// Jobs controls which users jobs may run as (SPEC.md §7.4).
type Jobs struct {
	// DefaultUser runs jobs that do not ask for a --user.
	DefaultUser string `yaml:"default_user"`
	// AllowedUsers lists every user a job may run as.
	AllowedUsers []string `yaml:"allowed_users"`
}

type Loki struct {
	URL          string `yaml:"url"`
	User         string `yaml:"user"`
	PasswordFile string `yaml:"password_file"`
}

type Grafana struct {
	ExploreURL string `yaml:"explore_url"`
	// LokiDatasourceUID makes result links open the job's logs directly.
	// Optional; without it the link opens Explore and the comment shows
	// the LogQL query.
	LokiDatasourceUID string `yaml:"loki_datasource_uid"`
}

var (
	// UserRe matches the user names peon accepts: a conservative subset of
	// what shadow-utils allows, so a name is never ambiguous on a card.
	UserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func defaults() Config {
	return Config{Jobs: Jobs{DefaultUser: "root", AllowedUsers: []string{"root"}}, Limits: Limits{
		JobTTL:          Duration{30 * time.Minute},
		DefaultTimeout:  Duration{10 * time.Minute},
		MaxTimeout:      Duration{2 * time.Hour},
		MaxCommandBytes: 4096,
		OutputTailLines: 50,
		PollInterval:    Duration{10 * time.Second},
	}}
}

// Load parses the file at path, applies defaults and validates the result.
// Parsing is strict: unknown keys, duplicate keys and multiple YAML
// documents are errors, because a typo in a security setting must not be
// silently ignored.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse %s: file is empty", path)
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: expected a single YAML document", path)
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

	seen := map[string]bool{}
	for _, u := range c.Jobs.AllowedUsers {
		if !UserRe.MatchString(u) {
			bad("jobs.allowed_users: %q is not a valid user name", u)
		}
		if seen[u] {
			bad("jobs.allowed_users: %q listed twice", u)
		}
		seen[u] = true
	}
	if !seen[c.Jobs.DefaultUser] {
		bad("jobs.default_user %q must be in jobs.allowed_users", c.Jobs.DefaultUser)
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

// UserAllowed reports whether jobs may run as name.
func (c *Config) UserAllowed(name string) bool {
	for _, u := range c.Jobs.AllowedUsers {
		if u == name {
			return true
		}
	}
	return false
}

// LingerDir is where logind records users with lingering enabled.
const LingerDir = "/var/lib/systemd/linger"

// CheckHost verifies the parts of the configuration that depend on this
// host: every allowed user exists, and every non-root one has lingering
// enabled so their systemd user manager runs without a login. lookup
// returns an error for an unknown user; lingerDir is normally LingerDir.
func (c *Config) CheckHost(lookup func(string) error, lingerDir string) error {
	var errs []error
	for _, u := range c.Jobs.AllowedUsers {
		if err := lookup(u); err != nil {
			errs = append(errs, fmt.Errorf("jobs.allowed_users: user %q: %w", u, err))
			continue
		}
		if u == "root" {
			continue
		}
		if _, err := os.Stat(filepath.Join(lingerDir, u)); err != nil {
			errs = append(errs, fmt.Errorf("jobs.allowed_users: user %q has no lingering systemd user manager; run: loginctl enable-linger %s", u, u))
		}
	}
	return errors.Join(errs...)
}
