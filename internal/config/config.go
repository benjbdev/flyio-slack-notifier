package config

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Fly          FlyConfig    `yaml:"fly"`
	Apps         []AppConfig  `yaml:"apps"`
	Slack        SlackConfig  `yaml:"slack"`
	PollInterval Duration     `yaml:"poll_interval"`
	DedupWindow  Duration     `yaml:"dedup_window"`
	StateFile    string       `yaml:"state_file"`
	Digest       DigestConfig `yaml:"digest"`
}

type FlyConfig struct {
	APIToken string `yaml:"api_token"`
	BaseURL  string `yaml:"base_url"`
}

type AppConfig struct {
	Name string `yaml:"name"`
}

type SlackConfig struct {
	DefaultWebhook string            `yaml:"default_webhook"`
	Routing        map[string]string `yaml:"routing"`
}

type DigestConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Schedule string `yaml:"schedule"`
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Get() time.Duration { return time.Duration(d) }

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := expandEnv(string(raw))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var envVarRe = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		return os.Getenv(name)
	})
}

func applyDefaults(c *Config) {
	if c.Fly.BaseURL == "" {
		c.Fly.BaseURL = "https://api.machines.dev"
	}
	if c.PollInterval.Get() == 0 {
		c.PollInterval = Duration(30 * time.Second)
	}
	if c.DedupWindow.Get() == 0 {
		c.DedupWindow = Duration(5 * time.Minute)
	}
	if c.StateFile == "" {
		c.StateFile = "./notifier.db"
	}
	if c.Digest.Schedule == "" {
		c.Digest.Schedule = "0 * * * *"
	}
}

func validate(c *Config) error {
	if c.Fly.APIToken == "" {
		return fmt.Errorf("fly.api_token is required (set FLY_API_TOKEN)")
	}
	if c.Slack.DefaultWebhook == "" {
		return fmt.Errorf("slack.default_webhook is required (set SLACK_WEBHOOK_FLY_NOTIF)")
	}
	if len(c.Apps) == 0 {
		return fmt.Errorf("at least one app must be configured under apps:")
	}
	for i, app := range c.Apps {
		if strings.TrimSpace(app.Name) == "" {
			return fmt.Errorf("apps[%d].name is empty", i)
		}
	}
	return nil
}

// LoadDotenv reads a simple KEY=VALUE file and sets each entry in the
// process environment if it isn't already set. Lines starting with '#'
// and blank lines are ignored. Surrounding quotes on the value are
// stripped. Missing file is not an error.
func LoadDotenv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return scanner.Err()
}
