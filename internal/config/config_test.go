package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("secret\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	token := writeSecret(t, dir, "forgejo.token", 0o600)
	pass := writeSecret(t, dir, "loki.password", 0o600)
	body = strings.ReplaceAll(body, "@TOKEN@", token)
	body = strings.ReplaceAll(body, "@PASS@", pass)
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `
host = "db1"

[forgejo]
url = "https://forgejo.internal"
repos = ["ops/infra"]
bot_user_id = 31
bot_token_file = "@TOKEN@"
approver_id = 1
requester_ids = [7, 8]

[limits]
job_ttl = "15m"

[loki]
url = "https://loki.internal"
user = "peon-db1"
password_file = "@PASS@"

[grafana]
explore_url = "https://grafana.internal/explore"
`

func TestLoadValid(t *testing.T) {
	c, err := Load(writeConfig(t, valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Limits.JobTTL.Minutes() != 15 {
		t.Errorf("job_ttl = %s, want 15m", c.Limits.JobTTL)
	}
	if c.Limits.MaxCommandBytes != 4096 {
		t.Errorf("default max_command_bytes not applied: %d", c.Limits.MaxCommandBytes)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]struct {
		from, to, want string
	}{
		"approver is bot":    {"approver_id = 1", "approver_id = 31", "must differ from bot_user_id"},
		"bot is requester":   {"requester_ids = [7, 8]", "requester_ids = [7, 31]", "must not contain bot_user_id"},
		"unknown key":        {"job_ttl = \"15m\"", "job_ttl = \"15m\"\njob_tll = \"1m\"", "unknown keys"},
		"bad host":           {`host = "db1"`, `host = "DB_1"`, "lowercase DNS label"},
		"bad repo":           {`repos = ["ops/infra"]`, `repos = ["infra"]`, "not owner/name"},
		"timeout over max":   {"job_ttl = \"15m\"", "job_ttl = \"15m\"\ndefault_timeout = \"3h\"", "exceeds limits.max_timeout"},
		"non-http loki":      {`url = "https://loki.internal"`, `url = "loki.internal"`, "loki.url"},
		"missing token file": {`bot_token_file = "@TOKEN@"`, `bot_token_file = "/nonexistent/token"`, "bot_token_file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, strings.Replace(valid, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSecretFilePermissions(t *testing.T) {
	p := writeConfig(t, valid)
	token := filepath.Join(filepath.Dir(p), "forgejo.token")
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "too open") {
		t.Fatalf("error = %v, want permission error", err)
	}
}
