package commit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

func terminalInput(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

type setupUI struct {
	in          io.Reader
	out         io.Writer
	interactive bool
	auth        string
	secret      func() (string, error)
	login       func(context.Context) error
}

func setupCommand(ctx context.Context, c Config, args []string, in io.Reader, out, errOut io.Writer, version string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(out)
	yes := fs.Bool("yes", false, "reuse existing credentials without prompting")
	auth := fs.String("auth", "", "replace authentication: api or cli")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: grok-commit setup [--yes|--auth api|cli]")
	}
	if *auth != "" && (*auth != "api" && *auth != "cli" || *yes) {
		return errors.New("use setup --auth api or setup --auth cli in a terminal")
	}
	ui := setupUI{in: in, out: out, interactive: !*yes && terminalInput(in), auth: *auth}
	ui.secret = func() (string, error) {
		f, ok := in.(*os.File)
		if !ok {
			return "", errors.New("a terminal is required for hidden API-key entry")
		}
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		return strings.TrimSpace(string(b)), err
	}
	ui.login = func(ctx context.Context) error {
		if _, err := exec.LookPath("grok"); err != nil {
			return errors.New("Grok CLI is not installed; use an xAI API key with grok-commit setup instead")
		}
		cmd := exec.CommandContext(ctx, "grok", "login")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
		return cmd.Run()
	}
	if err := configureAuth(ctx, c, ui); err != nil {
		return err
	}
	target, err := executablePath()
	if err != nil {
		return err
	}
	if _, err := versionParts(version); err != nil || packageManaged(target) {
		fmt.Fprintln(out, "Updates: use your source build or package manager.")
	} else {
		if err := registerInstallation(c, target, version); err != nil {
			return err
		}
		interval, err := updateInterval(c)
		if err != nil {
			return err
		}
		if updatesEnabled(c) {
			fmt.Fprintf(out, "Updates: automatic, checked after use every %g days; silent on failure.\n", interval.Hours()/24)
			fmt.Fprintln(out, "Turn off anytime: grok-commit update --disable")
		} else {
			fmt.Fprintln(out, "Updates: automatic updates are disabled.")
		}
	}
	fmt.Fprintln(out, "\nReady! In a Git repository, run: grok-commit -a")
	return nil
}

// Only persist a newly entered key after the provider has accepted it. Setup
// validates authentication with GET /models; it never generates or commits.
func configureAuth(ctx context.Context, c Config, ui setupUI) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("Git is required. Install it from https://git-scm.com/downloads, then run grok-commit setup")
	}
	fmt.Fprintln(ui.out, "Setting up grok-commit...")
	probe := c
	resolveErr := probe.Resolve()
	newKey := ""
	if resolveErr != nil || ui.auth != "" {
		if !ui.interactive {
			if resolveErr == nil {
				resolveErr = errors.New("changing authentication requires a terminal")
			}
			return fmt.Errorf("%w; run grok-commit setup in a terminal for guided setup", resolveErr)
		}
		choice := ui.auth
		if choice == "" {
			fmt.Fprintln(ui.out, "1) xAI API key — create one at https://console.x.ai (API billing applies)")
			fmt.Fprintln(ui.out, "2) Sign in with an installed Grok CLI")
			fmt.Fprint(ui.out, "Choose [1]: ")
			var err error
			choice, err = bufio.NewReader(ui.in).ReadString('\n')
			if err != nil {
				return errors.New("setup cancelled; run grok-commit setup when ready")
			}
		}
		switch strings.TrimSpace(choice) {
		case "", "1", "api":
			fmt.Fprint(ui.out, "xAI API key (hidden): ")
			var err error
			newKey, err = ui.secret()
			if err != nil {
				return err
			}
			if newKey == "" || len(newKey) > 8192 || strings.ContainsAny(newKey, "\r\n\t ") {
				return errors.New("expected one API key; nothing was saved")
			}
			probe = c
			probe.Auth, probe.keyOverride = "api", newKey
		case "2", "cli":
			if err := ui.login(ctx); err != nil {
				return err
			}
			probe = c
			probe.Auth = "cli"
		default:
			return errors.New("choose 1 or 2; run grok-commit setup again")
		}
		if err := probe.Resolve(); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(ui.out, "Reusing existing Grok authentication (%s).\n", probe.Auth)
	}
	g, err := NewGrok(ctx, probe)
	if err != nil {
		return err
	}
	defer g.Close()
	fmt.Fprintln(ui.out, "Checking Grok connection...")
	if err := g.Warm(ctx); err != nil {
		return fmt.Errorf("%w; check your connection, or reconnect with setup --auth api / setup --auth cli", err)
	}
	if newKey != "" {
		data, _ := json.Marshal(map[string]string{"api_key": newKey})
		if err := atomicWrite(filepath.Join(c.ConfigDir, "credentials.json"), data); err != nil {
			return err
		}
		if err := setConfigValue(c, "auth", "api"); err != nil {
			return err
		}
	} else if resolveErr != nil || ui.auth != "" {
		if err := setConfigValue(c, "auth", probe.Auth); err != nil {
			return err
		}
	}
	fmt.Fprintln(ui.out, "Git: available. Grok: connected.")
	return nil
}
