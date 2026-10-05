package commit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const apiURL = "https://api.x.ai/v1"
const cliURL = "https://cli-chat-proxy.grok.com/v1"

type Config struct {
	AutoUpdate     *bool         `json:"auto_update,omitempty"`
	UpdateInterval string        `json:"update_interval,omitempty"`
	Version        string        `json:"-"`
	Model          string        `json:"model"`
	Reasoning      string        `json:"reasoning"`
	Auth           string        `json:"auth"`
	BaseURL        string        `json:"base_url"`
	Timeout        string        `json:"timeout"`
	HedgeDelay     string        `json:"hedge_delay"`
	StateDir       string        `json:"-"`
	ConfigDir      string        `json:"-"`
	RequestTimeout time.Duration `json:"-"`
	Hedge          time.Duration `json:"-"`
	keyOverride    string
}

func configDir() (string, error) {
	if p := os.Getenv("GROK_COMMIT_CONFIG_DIR"); p != "" {
		return filepath.Abs(p)
	}
	p, err := os.UserConfigDir()
	return filepath.Join(p, "grok-commit"), err
}

func LoadConfig() (Config, error) {
	c := Config{Model: "grok-4.3", Auth: "auto", Timeout: "30s", HedgeDelay: "700ms"}
	dir, err := configDir()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		if err = json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("read config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	c.ConfigDir = dir
	for key, target := range map[string]*string{"MODEL": &c.Model, "REASONING": &c.Reasoning, "AUTH": &c.Auth, "BASE_URL": &c.BaseURL, "TIMEOUT": &c.Timeout, "HEDGE_DELAY": &c.HedgeDelay} {
		if v, ok := os.LookupEnv("GROK_COMMIT_" + key); ok {
			*target = v
		}
	}
	c.StateDir = os.Getenv("GROK_COMMIT_STATE_DIR")
	if c.StateDir == "" {
		base, e := os.UserCacheDir()
		if e != nil {
			return c, e
		}
		c.StateDir = filepath.Join(base, "grok-commit")
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	return c, err
}

func (c *Config) Resolve() error {
	if !strings.HasPrefix(c.Model, "grok-") {
		return errors.New("model must be a Grok model")
	}
	if c.Reasoning == "" {
		c.Reasoning = "low"
		if c.Model == "grok-4.3" {
			c.Reasoning = "none"
		}
	}
	switch c.Reasoning {
	case "none", "low", "medium", "high":
	default:
		return errors.New("invalid reasoning level")
	}
	var err error
	c.RequestTimeout, err = time.ParseDuration(c.Timeout)
	if err != nil || c.RequestTimeout <= 0 || c.RequestTimeout > 5*time.Minute {
		return errors.New("timeout must be between 0 and 5m")
	}
	c.Hedge, err = time.ParseDuration(c.HedgeDelay)
	if err != nil || c.Hedge < 0 {
		return errors.New("hedge_delay must be a nonnegative duration; 0 disables it")
	}
	if c.Auth == "auto" {
		if key, _ := c.apiKey(); key != "" {
			c.Auth = "api"
		} else {
			c.Auth = "cli"
		}
	}
	if c.Auth != "api" && c.Auth != "cli" {
		return errors.New("auth must be auto, api, or cli")
	}
	if c.BaseURL == "" {
		c.BaseURL = apiURL
		if c.Auth == "cli" {
			c.BaseURL = cliURL
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid API base URL")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("API base URL must use HTTPS (HTTP is allowed only on localhost)")
	}
	if c.Auth == "cli" && c.BaseURL != cliURL {
		return errors.New("Grok CLI credentials can only be sent to the Grok CLI endpoint")
	}
	_, err = c.credential()
	return err
}

func (c Config) apiKey() (string, error) {
	if c.keyOverride != "" {
		return c.keyOverride, nil
	}
	if key := strings.TrimSpace(os.Getenv("XAI_API_KEY")); key != "" {
		return key, nil
	}
	b, err := os.ReadFile(filepath.Join(c.ConfigDir, "credentials.json"))
	if err != nil {
		return "", err
	}
	var saved struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		return "", errors.New("invalid credentials file")
	}
	return strings.TrimSpace(saved.APIKey), nil
}

func (c Config) credential() (string, error) {
	if c.Auth == "api" {
		key, _ := c.apiKey()
		if key == "" {
			return "", errors.New("run grok-commit setup, or set XAI_API_KEY")
		}
		return key, nil
	}
	home := os.Getenv("GROK_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".grok")
	}
	b, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return "", errors.New("no Grok credentials: run grok-commit setup, or set XAI_API_KEY")
	}
	var entries map[string]struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(b, &entries) != nil {
		return "", errors.New("invalid Grok CLI authentication file")
	}
	var keys []string
	for name, entry := range entries {
		if strings.HasPrefix(name, "https://auth.x.ai::") && entry.Key != "" {
			keys = append(keys, entry.Key)
		}
	}
	if len(keys) != 1 {
		return "", errors.New("Grok CLI login is missing or ambiguous; sign in again or use XAI_API_KEY")
	}
	return keys[0], nil
}

func cliVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "grok", "--version").Output()
	if err != nil {
		return "", errors.New("Grok CLI authentication requires an installed grok command")
	}
	m := regexp.MustCompile(`\bgrok (\d+\.\d+\.\d+)`).FindSubmatch(b)
	if len(m) != 2 {
		return "", errors.New("cannot determine Grok CLI version")
	}
	return string(m[1]), nil
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state path must be a directory, not a symlink")
	}
	return os.Chmod(path, 0700)
}

func atomicWrite(path string, data []byte) error {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
