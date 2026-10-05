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
	path := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("Couldn't read settings from %s (%v). Fix the file, or delete it to use the defaults.", path, err)
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
		return fmt.Errorf("%q isn't a Grok model. grok-commit works only with Grok models, such as grok-4.3.", c.Model)
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
		return fmt.Errorf("Reasoning level %q isn't supported. Use none, low, medium, or high.", c.Reasoning)
	}
	var err error
	c.RequestTimeout, err = time.ParseDuration(c.Timeout)
	if err != nil || c.RequestTimeout <= 0 || c.RequestTimeout > 5*time.Minute {
		return fmt.Errorf("Timeout %q isn't valid. Use a duration above 0 and up to 5m, such as 30s or 2m.", c.Timeout)
	}
	c.Hedge, err = time.ParseDuration(c.HedgeDelay)
	if err != nil || c.Hedge < 0 {
		return fmt.Errorf("Hedge delay %q isn't valid. Use a duration such as 700ms or 2s, or 0 to turn off backup requests.", c.HedgeDelay)
	}
	auto := c.Auth == "auto"
	if auto {
		// A damaged key file or a custom endpoint shows the user meant API-key mode.
		key, keyErr := c.apiKey()
		c.Auth = "cli"
		if key != "" || errors.Is(keyErr, errDamagedKey) || c.BaseURL != "" && strings.TrimRight(c.BaseURL, "/") != cliURL {
			c.Auth = "api"
		}
	}
	if c.Auth != "api" && c.Auth != "cli" {
		return fmt.Errorf("Auth mode %q isn't supported. Use auto, api (xAI API key), or cli (Grok CLI sign-in).", c.Auth)
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
		return errors.New("The API address must be a full URL such as https://api.x.ai/v1, without a username, password, query string, or fragment.")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("The API address must start with https:// so your key is encrypted in transit (http:// is allowed only for localhost).")
	}
	if c.Auth == "cli" && c.BaseURL != cliURL {
		return errors.New("A custom API address (--base-url) works only with an xAI API key; Grok CLI sign-in always connects to Grok directly. Remove the custom address, or connect with an API key: grok-commit setup --auth api")
	}
	_, err = c.credential()
	if auto && errors.Is(err, errNoCLISignIn) {
		return errNotConnected
	}
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
		return "", errDamagedKey
	}
	return strings.TrimSpace(saved.APIKey), nil
}

func (c Config) credential() (string, error) {
	if c.Auth == "api" {
		key, err := c.apiKey()
		if errors.Is(err, errDamagedKey) {
			return "", err
		}
		if key == "" {
			return "", errNotConnected
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
		return "", errNoCLISignIn
	}
	var entries map[string]struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(b, &entries) != nil {
		return "", credentialsError("Grok CLI's sign-in file is damaged. Run grok login to sign in again, or connect with an API key: grok-commit setup --auth api")
	}
	var keys []string
	for name, entry := range entries {
		if strings.HasPrefix(name, "https://auth.x.ai::") && entry.Key != "" {
			keys = append(keys, entry.Key)
		}
	}
	switch len(keys) {
	case 0:
		return "", errNoCLISignIn
	case 1:
		return keys[0], nil
	}
	return "", credentialsError("Grok CLI is signed in to more than one account, so grok-commit can't tell which one to use. Connect with an API key instead: grok-commit setup --auth api")
}

func cliVersion(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("grok"); err != nil {
		return "", errNoGrokCLI
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "grok", "--version").Output()
	if err != nil {
		return "", errors.New("Grok CLI didn't respond to grok --version. Check that it runs, or connect with an API key: grok-commit setup --auth api")
	}
	m := regexp.MustCompile(`\bgrok (\d+\.\d+\.\d+)`).FindSubmatch(b)
	if len(m) != 2 {
		return "", errors.New("Couldn't read the Grok CLI version from grok --version. Update Grok CLI, or connect with an API key: grok-commit setup --auth api")
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
		return fmt.Errorf("%s must be a regular folder, not a file or symbolic link. Move it out of the way, then try again.", path)
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
