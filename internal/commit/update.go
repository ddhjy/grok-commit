package commit

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type installation struct {
	Path    string `json:"path"`
	Managed bool   `json:"managed"`
}
type updateState struct {
	LastCheck    time.Time `json:"last_check,omitempty"`
	NextCheck    time.Time `json:"next_check,omitempty"`
	Installed    string    `json:"installed,omitempty"`
	Previous     string    `json:"previous,omitempty"`
	PreviousHash string    `json:"previous_sha256,omitempty"`
	SkipVersion  string    `json:"skip_version,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

func updateInterval(c Config) (time.Duration, error) {
	s := c.UpdateInterval
	if s == "" {
		return 7 * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 1 || n > 365 {
			return 0, errors.New("update interval must be between 1d and 365d")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 24*time.Hour || d > 365*24*time.Hour {
		return 0, errors.New("update interval must be between 24h and 8760h")
	}
	return d, nil
}

func updatesEnabled(c Config) bool {
	if value, ok := os.LookupEnv("GROK_COMMIT_AUTO_UPDATE"); ok {
		return value != "0" && value != "false"
	}
	return c.AutoUpdate == nil || *c.AutoUpdate
}

func executablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func packageManaged(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	for _, part := range []string{"/cellar/", "/caskroom/", "/nix/store/", "/snap/", "/scoop/apps/", "/chocolatey/"} {
		if strings.Contains(p, part) {
			return true
		}
	}
	return strings.HasPrefix(p, "/usr/bin/") || strings.HasPrefix(p, "/bin/")
}

func readInstallation(c Config) (installation, error) {
	var installed installation
	b, err := os.ReadFile(filepath.Join(c.ConfigDir, "installation.json"))
	if err != nil {
		return installed, err
	}
	if err = json.Unmarshal(b, &installed); err != nil {
		return installed, err
	}
	if !installed.Managed || !filepath.IsAbs(installed.Path) || packageManaged(installed.Path) {
		return installed, errors.New("installation is managed by another package manager")
	}
	return installed, nil
}

func updateDir(c Config, target string) string {
	return filepath.Join(c.StateDir, "updates", digest([]byte(target))[:16])
}
func statePath(c Config, target string) string {
	return filepath.Join(updateDir(c, target), "state.json")
}

func readUpdateState(c Config, target string) (updateState, error) {
	var state updateState
	b, err := os.ReadFile(statePath(c, target))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(b, &state)
	return state, err
}

func saveUpdateState(c Config, target string, state updateState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(statePath(c, target), b)
}

func registerInstallation(c Config, target, version string) error {
	if _, err := versionParts(version); err != nil {
		return errors.New("automatic updates require an official release build")
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	if packageManaged(resolved) {
		return errors.New("use your package manager to update this installation")
	}
	b, _ := json.MarshalIndent(installation{resolved, true}, "", "  ")
	if err := atomicWrite(filepath.Join(c.ConfigDir, "installation.json"), b); err != nil {
		return err
	}
	state, err := readUpdateState(c, resolved)
	if err != nil {
		return err
	}
	interval, err := updateInterval(c)
	if err != nil {
		return err
	}
	if state.NextCheck.IsZero() {
		state.NextCheck = time.Now().Add(interval)
	}
	state.Installed = strings.TrimPrefix(version, "v")
	return saveUpdateState(c, resolved, state)
}

func setConfigValue(c Config, key string, value any) error {
	path := filepath.Join(c.ConfigDir, "config.json")
	fields := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(b, &fields); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fields[key] = b
	b, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

// MaybeAutoUpdate performs only local checks and a detached handoff. Network
// traffic and replacement never run on the commit's critical path.
func MaybeAutoUpdate(version string, args []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return
	}
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "--version" || a == "-v" {
			return
		}
	}
	if os.Getenv("CI") != "" || os.Getenv("GROK_COMMIT_UPDATE_WORKER") == "1" {
		return
	}
	if _, err := versionParts(version); err != nil {
		return
	}
	c, err := LoadConfig()
	if err != nil || !updatesEnabled(c) {
		return
	}
	target, err := executablePath()
	if err != nil {
		return
	}
	installed, err := readInstallation(c)
	if err != nil || installed.Path != target {
		return
	}
	state, err := readUpdateState(c, target)
	if err != nil || time.Now().Before(state.NextCheck) {
		return
	}
	_ = startUpdateWorker(c, target, "auto")
}

func updateLock(c Config, target string) (func(), error) {
	dir := updateDir(c, target)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	unlock, err := lockWorker(filepath.Join(dir, "update.lock"))
	if err != nil {
		return nil, errors.New("another update is already running")
	}
	return unlock, nil
}

func startUpdateWorker(c Config, target, action string) (err error) {
	unlock, err := updateLock(c, target)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := readUpdateState(c, target)
	if err != nil {
		return err
	}
	if action == "auto" && time.Now().Before(state.NextCheck) {
		return nil
	}
	interval, err := updateInterval(c)
	if err != nil {
		return err
	}
	// Claim this week's check before spawning, including failed starts, so a
	// network outage or concurrent shells cannot repeatedly wake the updater.
	state.LastCheck = time.Now()
	state.NextCheck = state.LastCheck.Add(interval)
	if err = saveUpdateState(c, target, state); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			state.LastError = err.Error()
			_ = saveUpdateState(c, target, state)
		}
	}()
	dir := updateDir(c, target)
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "helper-") {
			if info, e := entry.Info(); e == nil && time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	f, err := os.CreateTemp(dir, "helper-*")
	if err != nil {
		return err
	}
	helper := f.Name()
	f.Close()
	os.Remove(helper)
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if err = copyExecutable(target, helper); err != nil {
		return err
	}
	cmd := exec.Command(helper, "__update", target, action)
	cmd.Env = append(os.Environ(), "GROK_COMMIT_CONFIG_DIR="+c.ConfigDir, "GROK_COMMIT_STATE_DIR="+c.StateDir, "GROK_COMMIT_UPDATE_WORKER=1")
	detach(cmd)
	if err = cmd.Start(); err != nil {
		os.Remove(helper)
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func runUpdateWorker(ctx context.Context, c Config, args []string, version string) error {
	if len(args) != 2 || os.Getenv("GROK_COMMIT_UPDATE_WORKER") != "1" {
		return errors.New("internal updater must be started by grok-commit")
	}

	installed, err := readInstallation(c)
	if err != nil || installed.Path != args[0] {
		return errors.New("update target does not match the registered installation")
	}
	self, err := executablePath()
	helperDir, dirErr := filepath.EvalSymlinks(updateDir(c, installed.Path))
	if err != nil || dirErr != nil || filepath.Dir(self) != helperDir || !strings.HasPrefix(filepath.Base(self), "helper-") {
		return errors.New("updater must run from its private helper copy")
	}
	defer os.Remove(self)
	if args[1] != "auto" && args[1] != "manual" && args[1] != "rollback" {
		return errors.New("invalid update action")
	}
	if args[1] == "auto" && !updatesEnabled(c) {
		return nil
	}
	// The parent releases its handoff lock immediately after spawning.
	var unlock func()
	for i := 0; i < 100; i++ {
		unlock, err = updateLock(c, installed.Path)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	defer unlock()
	if args[1] == "rollback" {
		err = rollbackUpdate(ctx, c, installed.Path, version, newDistribution())
		if err != nil {
			state, _ := readUpdateState(c, installed.Path)
			state.LastError = err.Error()
			_ = saveUpdateState(c, installed.Path, state)
		}
		return err
	}
	_, err = performUpdate(ctx, c, installed.Path, version, false, args[1] == "auto", newDistribution())
	return err
}

func performUpdate(ctx context.Context, c Config, target, current string, checkOnly, automatic bool, d distribution) (message string, err error) {
	state, err := readUpdateState(c, target)
	if err != nil {
		return "", err
	}
	interval, err := updateInterval(c)
	if err != nil {
		return "", err
	}
	state.LastCheck = time.Now()
	state.NextCheck = state.LastCheck.Add(interval)
	state.LastError = ""
	defer func() {
		if err != nil {
			state.LastError = err.Error()
		}
		if saveErr := saveUpdateState(c, target, state); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("save update status: %w", saveErr))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := d.latest(ctx)
	if err != nil {
		return "", err
	}
	if !newer(r.Tag, current) {
		return "Already up to date (" + current + ").", nil
	}
	if automatic && state.SkipVersion == r.Tag {
		return "Skipped previously rolled-back version.", nil
	}
	if checkOnly {
		return "Available: " + r.Tag + " (installed: " + current + "). Run grok-commit update to install.", nil
	}
	// Respect changes to the installation while the background check was running.
	if err = d.verifyBinary(ctx, target, strings.TrimPrefix(current, "v")); err != nil {
		return "", errors.New("installation changed while checking; try again")
	}
	candidate, err := d.download(ctx, r, filepath.Dir(target))
	if err != nil {
		return "", err
	}
	defer os.Remove(candidate)
	if automatic {
		latestConfig, loadErr := LoadConfig()
		if loadErr != nil || !updatesEnabled(latestConfig) {
			return "Automatic updates disabled.", nil
		}
	}
	if err = d.verifyBinary(ctx, target, strings.TrimPrefix(current, "v")); err != nil {
		return "", errors.New("installation changed while downloading; try again")
	}
	previousHash, err := installCandidate(ctx, candidate, target)
	if err != nil {
		return "", fmt.Errorf("cannot complete executable replacement: %w", err)
	}
	state.Previous = strings.TrimPrefix(current, "v")
	state.PreviousHash = previousHash
	state.Installed = strings.TrimPrefix(r.Tag, "v")
	state.SkipVersion = ""
	return "Updated to " + r.Tag + ". Previous version retained for update --rollback.", nil
}

func rollbackUpdate(ctx context.Context, c Config, target, current string, d distribution) error {
	state, err := readUpdateState(c, target)
	if err != nil {
		return err
	}
	if state.Previous == "" || state.PreviousHash == "" {
		return errors.New("no previous version is available")
	}
	previous := previousPath(target)
	hash, err := fileHash(previous)
	if err != nil || hash != state.PreviousHash {
		return errors.New("previous version checksum mismatch")
	}
	if err = d.verifyBinary(ctx, previous, state.Previous); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".grok-commit-rollback-*")
	if err != nil {
		return err
	}
	candidate := f.Name()
	f.Close()
	os.Remove(candidate)
	defer os.Remove(candidate)
	if err = copyExecutable(previous, candidate); err != nil {
		return err
	}
	if err = replaceExecutable(ctx, candidate, target); err != nil {
		return err
	}
	state.Installed = state.Previous
	state.Previous = ""
	state.PreviousHash = ""
	state.SkipVersion = "v" + strings.TrimPrefix(current, "v")
	interval, err := updateInterval(c)
	if err != nil {
		return err
	}
	state.NextCheck = time.Now().Add(interval)
	state.LastError = ""
	return saveUpdateState(c, target, state)
}

func updateCommand(ctx context.Context, c Config, args []string, out io.Writer, version string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(out)
	check := fs.Bool("check", false, "check without installing")
	status := fs.Bool("status", false, "show update status")
	enable := fs.Bool("enable", false, "enable quiet background updates")
	disable := fs.Bool("disable", false, "disable background updates")
	rollback := fs.Bool("rollback", false, "restore previous version")
	interval := fs.String("interval", "", "check interval, e.g. 7d or 14d")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected update argument")
	}
	actions := 0
	for _, v := range []bool{*check, *status, *enable, *disable, *rollback, *interval != ""} {
		if v {
			actions++
		}
	}
	if actions > 1 {
		return errors.New("choose one update action")
	}
	if *disable {
		if err := setConfigValue(c, "auto_update", false); err != nil {
			return err
		}
		fmt.Fprintln(out, "Automatic updates disabled.")
		return nil
	}
	if *interval != "" {
		c.UpdateInterval = *interval
		if _, err := updateInterval(c); err != nil {
			return err
		}
		if err := setConfigValue(c, "update_interval", *interval); err != nil {
			return err
		}
		if installed, e := readInstallation(c); e == nil {
			unlock, e := updateLock(c, installed.Path)
			if e != nil {
				return e
			}
			defer unlock()
			state, e := readUpdateState(c, installed.Path)
			if e != nil {
				return e
			}
			d, _ := updateInterval(c)
			state.NextCheck = time.Now().Add(d)
			if e = saveUpdateState(c, installed.Path, state); e != nil {
				return e
			}
		}
		fmt.Fprintln(out, "Update interval:", *interval)
		return nil
	}
	target, err := executablePath()
	if err != nil {
		return err
	}
	if *enable {
		if err := registerInstallation(c, target, version); err != nil {
			return err
		}
		if err := setConfigValue(c, "auto_update", true); err != nil {
			return err
		}
		fmt.Fprintln(out, "Quiet background updates enabled (default interval: 7 days).")
		return nil
	}
	installed, err := readInstallation(c)
	if err != nil || installed.Path != target {
		return errors.New("run grok-commit setup to enable self-updates, or update through your package manager")
	}
	if *status {
		state, err := readUpdateState(c, target)
		if err != nil {
			return err
		}
		interval, err := updateInterval(c)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Installed: %s\nAutomatic updates: %t\nCheck interval: %g days\nNext check: %s\n", version, updatesEnabled(c), interval.Hours()/24, state.NextCheck.Local().Format(time.RFC3339))
		if state.Previous != "" {
			fmt.Fprintln(out, "Previous version:", state.Previous)
		}
		if state.LastError != "" {
			fmt.Fprintln(out, "Last check:", state.LastError)
		}
		return nil
	}
	if runtime.GOOS == "windows" && !*check {
		action := "manual"
		if *rollback {
			action = "rollback"
		}
		if err := startUpdateWorker(c, target, action); err != nil {
			return err
		}
		fmt.Fprintln(out, "Updating in the background. Use grok-commit update --status to check the result.")
		return nil
	}
	unlock, err := updateLock(c, target)
	if err != nil {
		return err
	}
	defer unlock()
	if *rollback {
		if err := rollbackUpdate(ctx, c, target, version, newDistribution()); err != nil {
			return err
		}
		fmt.Fprintln(out, "Restored the previous version. Automatic updates will skip the version you rolled back.")
		return nil
	}
	fmt.Fprintln(out, "Checking for updates...")
	message, err := performUpdate(ctx, c, target, version, *check, false, newDistribution())
	if err != nil {
		return err
	}
	fmt.Fprintln(out, message)
	return nil
}
