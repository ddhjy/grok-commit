package commit

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const serviceLabel = "com.grok-commit.daemon"

func loginServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}

// loginServiceInstalled reports whether launchd starts the service at login
// and restarts it whenever it stops.
func loginServiceInstalled() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	path, err := loginServicePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func service(ctx context.Context, c Config, remove bool) error {
	if runtime.GOOS != "darwin" {
		return notice{"Starting at login is available only on macOS. On Linux, the background service starts automatically when you commit; to keep it running, have your service manager (such as systemd) run: grok-commit daemon run --keepalive", nil}
	}
	path, err := loginServicePath()
	if err != nil {
		return err
	}
	if !remove && c.Auth == "api" {
		// launchd does not inherit the interactive shell's API key.
		if _, err := os.Stat(filepath.Join(c.ConfigDir, "credentials.json")); err != nil {
			return errors.New("Services that start at login can't read XAI_API_KEY from your shell. Save the key first with grok-commit setup --auth api, then run grok-commit daemon install again.")
		}
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+serviceLabel).Run()
	if err := stopDaemon(ctx, c); err != nil {
		return err
	}
	if remove {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := privateDir(c.StateDir); err != nil {
		return err
	}
	escape := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	env := map[string]string{"PATH": os.Getenv("PATH"), "GROK_COMMIT_CONFIG_DIR": c.ConfigDir, "GROK_COMMIT_STATE_DIR": c.StateDir,
		"GROK_COMMIT_AUTH": c.Auth, "GROK_COMMIT_BASE_URL": c.BaseURL, "GROK_COMMIT_TIMEOUT": c.Timeout, "GROK_COMMIT_HEDGE_DELAY": c.HedgeDelay}
	if value := os.Getenv("GROK_HOME"); value != "" {
		env["GROK_HOME"] = value
	}
	var environment bytes.Buffer
	for name, value := range env {
		fmt.Fprintf(&environment, "<key>%s</key><string>%s</string>", escape(name), escape(value))
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>daemon</string><string>run</string><string>--keepalive</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>30</integer>
<key>EnvironmentVariables</key><dict>%s</dict>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>`, serviceLabel, escape(exe), environment.String(), escape(filepath.Join(c.StateDir, "daemon.log")), escape(filepath.Join(c.StateDir, "daemon.log")))
	// Do not chmod the user's shared LaunchAgents directory.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0600); err != nil {
		return err
	}
	if b, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("macOS couldn't start the login service:\n%s\nRun grok-commit daemon uninstall, then try installing again.", indent(strings.TrimSpace(string(b))))
	}
	if err := waitDaemon(ctx, c); err != nil {
		return err
	}
	var result Result
	if err := localCall(ctx, c, "/warm", nil, &result); err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
