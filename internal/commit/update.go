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
	invalid := fmt.Errorf("Update interval %q isn't valid. Use a number of days from 1d to 365d, such as 14d.", s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 1 || n > 365 {
			return 0, invalid
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 24*time.Hour || d > 365*24*time.Hour {
		return 0, invalid
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

var errPackageManaged = notice{"This copy of grok-commit was installed with a package manager. Use that package manager to update it.", nil}

func sourceBuild(version string) error {
	return notice{fmt.Sprintf("This copy of grok-commit (version %s) isn't an official stable release, so it can't update itself. Update it the way you installed it, for example: go install github.com/ddhjy/grok-commit/cmd/grok-commit@latest", version), nil}
}

func notUpdatable(target, version string) error {
	if _, err := versionParts(version); err != nil {
		return sourceBuild(version)
	}
	if packageManaged(target) {
		return errPackageManaged
	}
	return notice{"Updates aren't set up for this copy of grok-commit. To set them up, run: grok-commit update --enable", nil}
}

func registerInstallation(c Config, target, version string) error {
	if _, err := versionParts(version); err != nil {
		return sourceBuild(version)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	if packageManaged(resolved) {
		return errPackageManaged
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
		return nil, errors.New("Another grok-commit update is already running. Wait a minute, then try again.")
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
			err = errors.Join(err, fmt.Errorf("Couldn't save the update status: %w", saveErr))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := d.latest(ctx)
	if err != nil {
		return "", err
	}
	latest, installed := strings.TrimPrefix(r.Tag, "v"), strings.TrimPrefix(current, "v")
	if !newer(r.Tag, current) {
		return "✓ You have the latest version (" + installed + ").", nil
	}
	if automatic && state.SkipVersion == r.Tag {
		return "Skipped " + latest + " because you rolled it back.", nil
	}
	if checkOnly {
		return fmt.Sprintf("Version %s is available (you have %s). To install it, run: grok-commit update", latest, installed), nil
	}
	changed := errors.New("grok-commit was replaced while the update was running (perhaps by another install), so nothing was changed. Run grok-commit update again.")
	// Respect changes to the installation while the background check was running.
	if err = d.verifyBinary(ctx, target, installed); err != nil {
		return "", changed
	}
	candidate, err := d.download(ctx, r, filepath.Dir(target))
	if err != nil {
		return "", err
	}
	defer os.Remove(candidate)
	if automatic {
		latestConfig, loadErr := LoadConfig()
		if loadErr != nil || !updatesEnabled(latestConfig) {
			return "Automatic updates were turned off, so the download wasn't installed.", nil
		}
	}
	if err = d.verifyBinary(ctx, target, installed); err != nil {
		return "", changed
	}
	previousHash, err := installCandidate(ctx, candidate, target)
	if err != nil {
		return "", fmt.Errorf("Couldn't install the update in %s (%v). Check that you can write to that folder, then try again.", filepath.Dir(target), err)
	}
	state.Previous = installed
	state.PreviousHash = previousHash
	state.Installed = latest
	state.SkipVersion = ""
	return fmt.Sprintf("✓ Updated to %s. To go back to %s, run: grok-commit update --rollback", latest, installed), nil
}

var errNoPreviousVersion = notice{"There's no earlier version to go back to yet. grok-commit keeps the previous version after each update.", nil}

func rollbackUpdate(ctx context.Context, c Config, target, current string, d distribution) error {
	state, err := readUpdateState(c, target)
	if err != nil {
		return err
	}
	if state.Previous == "" || state.PreviousHash == "" {
		return errNoPreviousVersion
	}
	previous := previousPath(target)
	hash, err := fileHash(previous)
	if err != nil || hash != state.PreviousHash {
		return errors.New("The saved previous version is missing or has been modified, so it wasn't restored.")
	}
	if err = d.verifyBinary(ctx, previous, state.Previous); err != nil {
		return errors.New("The saved previous version failed its version check, so it wasn't restored.")
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
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "")
	status := fs.Bool("status", false, "")
	enable := fs.Bool("enable", false, "")
	disable := fs.Bool("disable", false, "")
	rollback := fs.Bool("rollback", false, "")
	interval := fs.String("interval", "", "")
	if err := fs.Parse(args); err != nil {
		return flagError(err, "grok-commit update", fs, args)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("Unknown argument %q. Run grok-commit update --help to see the options.", fs.Arg(0))
	}
	var given []string
	fs.Visit(func(f *flag.Flag) { given = append(given, "--"+f.Name) })
	if len(given) > 1 {
		return fmt.Errorf("Use one option at a time (you gave %s).", strings.Join(given, ", "))
	}
	if *disable {
		if err := setConfigValue(c, "auto_update", false); err != nil {
			return err
		}
		fmt.Fprintln(out, "✓ Automatic updates are off. You can still update anytime with grok-commit update.")
		updateOverride(out, false)
		return nil
	}
	if *interval != "" {
		c.UpdateInterval = *interval
		every, err := updateInterval(c)
		if err != nil {
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
			state.NextCheck = time.Now().Add(every)
			if e = saveUpdateState(c, installed.Path, state); e != nil {
				return e
			}
		}
		fmt.Fprintf(out, "✓ grok-commit will check for updates %s.\n", frequency(every))
		if !updatesEnabled(c) {
			fmt.Fprintln(out, "Automatic updates are off, so this applies once you turn them on: grok-commit update --enable")
		}
		return nil
	}
	target, err := executablePath()
	if err != nil {
		return err
	}
	if *status {
		return updateStatus(c, target, version, out)
	}
	if *enable {
		if err := registerInstallation(c, target, version); err != nil {
			return err
		}
		if err := setConfigValue(c, "auto_update", true); err != nil {
			return err
		}
		every, err := updateInterval(c)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Automatic updates are on. grok-commit checks %s, in the background after you use it.\n", frequency(every))
		updateOverride(out, true)
		return nil
	}
	installed, err := readInstallation(c)
	if err != nil || installed.Path != target {
		return notUpdatable(target, version)
	}
	var previous string
	if *rollback {
		state, err := readUpdateState(c, target)
		if err != nil {
			return err
		}
		if state.Previous == "" || state.PreviousHash == "" {
			return errNoPreviousVersion
		}
		previous = state.Previous
	}
	if runtime.GOOS == "windows" && !*check {
		action, doing := "manual", "Updating"
		if *rollback {
			action, doing = "rollback", "Going back to version "+previous
		}
		if err := startUpdateWorker(c, target, action); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s in the background; this takes a few seconds.\nTo see the result, run: grok-commit update --status\n", doing)
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
		fmt.Fprintf(out, "✓ Went back to version %s. Automatic updates will skip %s, but grok-commit update can still install it.\n", previous, version)
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

// updatesSummary is setup's line about automatic updates. When
// GROK_COMMIT_AUTO_UPDATE decides, update --enable and --disable can't change
// the outcome, so it names the variable instead of suggesting them.
func updatesSummary(c Config, interval time.Duration) string {
	on := updatesEnabled(c)
	value, env := os.LookupEnv("GROK_COMMIT_AUTO_UPDATE")
	schedule := fmt.Sprintf("grok-commit checks %s, in the background after you use it, and stays quiet if a check fails.", frequency(interval))
	switch {
	case env && on:
		return fmt.Sprintf("Automatic updates: on (set by GROK_COMMIT_AUTO_UPDATE=%s in this shell). %s", value, schedule)
	case env:
		return fmt.Sprintf("Automatic updates: off (set by GROK_COMMIT_AUTO_UPDATE=%s in this shell).", value)
	case on:
		return "Automatic updates: on. " + schedule + "\nTo turn them off: grok-commit update --disable"
	}
	return "Automatic updates: off. To turn them on: grok-commit update --enable"
}

// updateOverride warns when GROK_COMMIT_AUTO_UPDATE outranks the setting the
// user just saved.
func updateOverride(out io.Writer, want bool) {
	if value, ok := os.LookupEnv("GROK_COMMIT_AUTO_UPDATE"); ok && updatesEnabled(Config{}) != want {
		fmt.Fprintf(out, "Note: GROK_COMMIT_AUTO_UPDATE=%s in this shell overrides this setting until you remove it from your environment.\n", value)
	}
}

func updateStatus(c Config, target, version string, out io.Writer) error {
	fmt.Fprintln(out, "Version:", version)
	installed, err := readInstallation(c)
	if err != nil || installed.Path != target {
		switch _, versionErr := versionParts(version); {
		case versionErr != nil:
			fmt.Fprintln(out, "Automatic updates: not available for builds from source")
		case packageManaged(target):
			fmt.Fprintln(out, "Automatic updates: handled by your package manager")
		default:
			fmt.Fprintln(out, "Automatic updates: not set up (to turn them on, run grok-commit update --enable)")
		}
		return nil
	}
	state, err := readUpdateState(c, target)
	if err != nil {
		return err
	}
	interval, err := updateInterval(c)
	if err != nil {
		return err
	}
	on := updatesEnabled(c)
	switch value, env := os.LookupEnv("GROK_COMMIT_AUTO_UPDATE"); {
	case env && on:
		fmt.Fprintf(out, "Automatic updates: on (set by GROK_COMMIT_AUTO_UPDATE=%s in this shell)\n", value)
	case env:
		fmt.Fprintf(out, "Automatic updates: off (set by GROK_COMMIT_AUTO_UPDATE=%s in this shell)\n", value)
	case on:
		fmt.Fprintln(out, "Automatic updates: on")
	default:
		fmt.Fprintln(out, "Automatic updates: off (to turn them on, run grok-commit update --enable)")
	}
	fmt.Fprintln(out, "Check interval:", days(interval))
	if on {
		next := "the next time you use grok-commit"
		if time.Now().Before(state.NextCheck) {
			next = "the first time you use grok-commit after " + state.NextCheck.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintln(out, "Next check:", next)
	}
	if state.Previous != "" {
		fmt.Fprintf(out, "Previous version: %s (to go back, run grok-commit update --rollback)\n", state.Previous)
	}
	if state.LastError != "" {
		fmt.Fprintln(out, "Last attempt failed:", state.LastError)
	}
	return nil
}
