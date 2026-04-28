package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("FOO", "bar")
	t.Setenv("FLY_API_TOKEN", "tk_abc")
	t.Setenv("EMPTY", "")

	cases := []struct {
		in, want string
	}{
		{"hello ${FOO}", "hello bar"},
		{"${FLY_API_TOKEN}", "tk_abc"},
		{"${MISSING}", ""},
		{"${EMPTY}", ""},
		{"no vars here", "no vars here"},
		{"${FOO}-${FOO}", "bar-bar"},
		{"$FOO ${FOO}", "$FOO bar"}, // bare $VAR not supported, only ${VAR}
	}
	for _, c := range cases {
		if got := expandEnv(c.in); got != c.want {
			t.Errorf("expandEnv(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	contents := `# a comment

FOO=bar
QUOTED="hello world"
SQUOTED='single'
SPACED = padded
EMPTYLINE_BELOW=

EXISTING=should_not_overwrite
`
	if err := os.WriteFile(p, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXISTING", "preset")

	if err := LoadDotenv(p); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"FOO":             "bar",
		"QUOTED":          "hello world",
		"SQUOTED":         "single",
		"SPACED":          "padded",
		"EMPTYLINE_BELOW": "",
		"EXISTING":        "preset",
	}
	for k, want := range checks {
		got, _ := os.LookupEnv(k)
		if got != want {
			t.Errorf("%s = %q; want %q", k, got, want)
		}
	}
}

func TestLoadDotenvMissingIsNoError(t *testing.T) {
	if err := LoadDotenv(filepath.Join(t.TempDir(), "no-such-file")); err != nil {
		t.Fatalf("missing .env should not error: %v", err)
	}
}

func TestLoadConfigOK(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "tk_test")
	t.Setenv("SLACK_WEBHOOK_FLY_NOTIF", "https://hooks.slack.com/services/x/y/z")

	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
fly:
  api_token: ${FLY_API_TOKEN}
apps:
  - name: api-prod
slack:
  default_webhook: ${SLACK_WEBHOOK_FLY_NOTIF}
poll_interval: 15s
dedup_window: 1m
digest:
  enabled: true
  schedule: "*/5 * * * *"
`
	if err := os.WriteFile(p, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Fly.APIToken != "tk_test" {
		t.Errorf("api_token: got %q", cfg.Fly.APIToken)
	}
	if cfg.Slack.DefaultWebhook != "https://hooks.slack.com/services/x/y/z" {
		t.Errorf("webhook: got %q", cfg.Slack.DefaultWebhook)
	}
	if cfg.PollInterval.Get() != 15*time.Second {
		t.Errorf("poll_interval: got %v", cfg.PollInterval.Get())
	}
	if cfg.DedupWindow.Get() != time.Minute {
		t.Errorf("dedup_window: got %v", cfg.DedupWindow.Get())
	}
	if cfg.Fly.BaseURL == "" {
		t.Errorf("default base_url should be applied")
	}
	if !cfg.Digest.Enabled {
		t.Errorf("digest.enabled should be true")
	}
}

func TestLoadConfigMissingToken(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "")
	t.Setenv("SLACK_WEBHOOK_FLY_NOTIF", "https://hooks.slack.com/services/x/y/z")

	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
fly:
  api_token: ${FLY_API_TOKEN}
apps:
  - name: foo
slack:
  default_webhook: ${SLACK_WEBHOOK_FLY_NOTIF}
`
	_ = os.WriteFile(p, []byte(yaml), 0600)
	if _, err := Load(p); err == nil {
		t.Errorf("expected error for missing token")
	}
}

func TestLoadConfigNoApps(t *testing.T) {
	t.Setenv("FLY_API_TOKEN", "tk_test")
	t.Setenv("SLACK_WEBHOOK_FLY_NOTIF", "https://hooks.slack.com/services/x/y/z")

	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
fly:
  api_token: ${FLY_API_TOKEN}
slack:
  default_webhook: ${SLACK_WEBHOOK_FLY_NOTIF}
`
	_ = os.WriteFile(p, []byte(yaml), 0600)
	if _, err := Load(p); err == nil {
		t.Errorf("expected error for missing apps")
	}
}
