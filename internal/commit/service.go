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
)

func service(ctx context.Context, c Config, remove bool) error {
	if runtime.GOOS != "darwin" {
		return errors.New("login service installation currently supports macOS; use daemon start on Linux")
	}
	const label = "com.grok-commit.daemon"
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+label).Run()
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
	if c.Auth == "api" {
		saved := c
		saved.ConfigDir = c.ConfigDir
		// launchd does not inherit the interactive shell's API key.
		if _, err := os.Stat(filepath.Join(saved.ConfigDir, "credentials.json")); err != nil {
			return errors.New("save your API key with auth --stdin before installing a login service")
		}
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
</dict></plist>`, label, escape(exe), environment.String(), escape(filepath.Join(c.StateDir, "daemon.log")), escape(filepath.Join(c.StateDir, "daemon.log")))
	// Do not chmod the user's shared LaunchAgents directory.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0600); err != nil {
		return err
	}
	if b, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("install service: %s", b)
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
