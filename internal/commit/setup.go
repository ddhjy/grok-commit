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
	retry       string // the commit command that opened setup, if any
	secret      func() (string, error)
	login       func(context.Context) error
	lines       *bufio.Reader
}

func setupCommand(ctx context.Context, c Config, args []string, retry string, in io.Reader, out, errOut io.Writer, version string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "")
	auth := fs.String("auth", "", "")
	if err := fs.Parse(args); err != nil {
		return flagError(err, "grok-commit setup", fs, args)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("Unknown argument %q. Run grok-commit setup --help to see the options.", fs.Arg(0))
	}
	if *auth != "" && *auth != "api" && *auth != "cli" {
		return fmt.Errorf("--auth %s isn't an option. Use --auth api (xAI API key) or --auth cli (Grok CLI sign-in).", *auth)
	}
	if *auth != "" && *yes {
		return fmt.Errorf("--yes only checks your current connection, so it can't be combined with --auth. To switch how grok-commit connects, run grok-commit setup --auth %s in a terminal.", *auth)
	}
	ui := setupUI{in: in, out: out, interactive: !*yes && terminalInput(in), auth: *auth, retry: retry}
	ui.secret = func() (string, error) {
		f, ok := in.(*os.File)
		if !ok {
			return "", errors.New("Entering an API key needs a terminal. To save a key from a script, pipe it in: printf '%s' \"$XAI_API_KEY\" | grok-commit auth --stdin")
		}
		// Disable terminal echo before displaying the prompt, including when
		// the user pastes immediately. ReadPassword also handles Ctrl-C safely.
		state, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return "", err
		}
		defer term.Restore(int(f.Fd()), state)
		terminal := term.NewTerminal(struct {
			io.Reader
			io.Writer
		}{f, out}, "")
		key, err := terminal.ReadPassword("Paste your xAI API key (input stays hidden): ")
		if errors.Is(err, io.EOF) {
			// Ctrl-C or Ctrl-D; the terminal is still raw, so end the line by hand.
			fmt.Fprint(out, "\r\n")
			return "", errSetupCancelled
		}
		return strings.TrimSpace(key), err
	}
	ui.login = func(ctx context.Context) error {
		if _, err := exec.LookPath("grok"); err != nil {
			return errNoGrokCLI
		}
		fmt.Fprintln(out, "Running grok login...")
		cmd := exec.CommandContext(ctx, "grok", "login")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return errSetupCancelled
			}
			return fmt.Errorf("grok login didn't finish (%v). Sign in with grok login, then run grok-commit setup --auth cli.", err)
		}
		return nil
	}
	if err := configureAuth(ctx, c, ui); err != nil {
		return err
	}
	target, err := executablePath()
	if err != nil {
		return err
	}
	switch _, versionErr := versionParts(version); {
	case versionErr != nil:
		fmt.Fprintln(out, "Automatic updates: not available for builds from source. Update grok-commit the way you installed it.")
	case packageManaged(target):
		fmt.Fprintln(out, "Automatic updates: handled by your package manager.")
	default:
		if err := registerInstallation(c, target, version); err != nil {
			return err
		}
		interval, err := updateInterval(c)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, updatesSummary(c, interval))
	}
	if _, err := (Repository{}).git(ctx, "rev-parse", "--is-inside-work-tree"); retry != "" && err == nil {
		fmt.Fprintf(out, "\nYou're all set. Run your command again: %s\n", retry)
	} else {
		fmt.Fprintln(out, "\nYou're all set. In a Git repository with changes, run: grok-commit -a")
	}
	return nil
}

// Only persist a newly entered key after the provider has accepted it. Setup
// validates authentication with GET /models; it never generates or commits.
func configureAuth(ctx context.Context, c Config, ui setupUI) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errGitMissing
	}
	if ui.lines == nil && ui.in != nil {
		ui.lines = bufio.NewReader(ui.in)
	}
	probe := c
	if ui.auth != "" {
		probe.Auth = ui.auth
	}
	resolveErr := probe.Resolve()
	if resolveErr != nil && !errors.As(resolveErr, new(credentialsError)) {
		return resolveErr
	}
	if resolveErr == nil && ui.auth == "" {
		fmt.Fprintf(ui.out, "Using %s.\n", credentialSource(probe))
		return ui.check(ctx, probe)
	}
	if !ui.interactive {
		if resolveErr == nil {
			return fmt.Errorf("Switching how grok-commit connects needs a terminal. Run grok-commit setup --auth %s in a terminal.", ui.auth)
		}
		return notice{resolveErr.Error(), resolveErr}
	}
	choice := ui.auth
	if choice == "" {
		if ui.retry != "" {
			fmt.Fprintln(ui.out, "Before grok-commit can write your commit subject, it needs to connect to Grok. This takes about a minute, and nothing has been staged or committed yet.")
		} else {
			fmt.Fprintln(ui.out, "Let's connect grok-commit to Grok. This takes about a minute.")
		}
		var err error
		if choice, err = ui.choose(ctx); err != nil {
			return err
		}
	}
	newKey := ""
	if choice == "api" {
		var err error
		if newKey, err = ui.enterKey(ctx, c); err != nil {
			return err
		}
		probe = c
		probe.Auth = "api"
		data, _ := json.Marshal(map[string]string{"api_key": newKey})
		if err := atomicWrite(filepath.Join(c.ConfigDir, "credentials.json"), data); err != nil {
			return err
		}
	} else {
		var err error
		if probe, err = ui.signIn(ctx, c); err != nil {
			return err
		}
	}
	if err := setConfigValue(c, "auth", probe.Auth); err != nil {
		return err
	}
	if newKey != "" {
		fmt.Fprintf(ui.out, "Your API key is saved in %s, outside your repositories.\n", c.ConfigDir)
		if key := strings.TrimSpace(os.Getenv("XAI_API_KEY")); key != "" && key != newKey {
			fmt.Fprintln(ui.out, "Note: XAI_API_KEY in this shell takes priority over the saved key. Remove it from your environment to use the saved key.")
		}
	}
	if auth := os.Getenv("GROK_COMMIT_AUTH"); auth != "" && auth != "auto" && auth != probe.Auth {
		fmt.Fprintf(ui.out, "Note: GROK_COMMIT_AUTH=%s in this shell overrides this choice. Remove it from your environment to use your %s.\n", auth, authLabel(probe.Auth))
	}
	return nil
}

func credentialSource(c Config) string {
	switch {
	case c.Auth == "cli":
		return "your Grok CLI sign-in"
	case strings.TrimSpace(os.Getenv("XAI_API_KEY")) != "":
		return "the xAI API key in XAI_API_KEY"
	}
	return "your saved xAI API key"
}

func (ui setupUI) check(ctx context.Context, c Config) error {
	g, err := NewGrok(ctx, c)
	if err != nil {
		return err
	}
	defer g.Close()
	fmt.Fprint(ui.out, "Checking the connection to Grok... ")
	if err := g.Warm(ctx); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(ui.out)
			return errSetupCancelled
		}
		fmt.Fprintln(ui.out, "failed")
		return err
	}
	fmt.Fprintln(ui.out, "✓")
	return nil
}

// choose asks how to connect and returns "api" or "cli".
func (ui setupUI) choose(ctx context.Context) (string, error) {
	_, err := exec.LookPath("grok")
	hasCLI := err == nil
	fmt.Fprintln(ui.out, "\nHow would you like to connect?")
	fmt.Fprintln(ui.out, "  1) xAI API key: create one at https://console.x.ai (usage is billed to your xAI account)")
	if hasCLI {
		fmt.Fprintln(ui.out, "  2) Grok CLI: use the account you signed in to with grok login")
	} else {
		fmt.Fprintln(ui.out, "  2) Grok CLI: not installed on this computer")
	}
	for try := 1; ; try++ {
		fmt.Fprint(ui.out, "Choose 1 or 2 [1]: ")
		answer, err := ui.readLine(ctx)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(answer) {
		case "", "1", "api":
			return "api", nil
		case "2", "cli":
			if hasCLI {
				return "cli", nil
			}
			if try == 3 {
				return "", errNoGrokCLI
			}
			fmt.Fprintln(ui.out, "Grok CLI isn't installed on this computer. Choose 1 to use an API key instead, or install Grok CLI and run grok-commit setup again.")
			continue
		}
		if try == 3 {
			return "", notice{"Setup stopped because neither 1 nor 2 was chosen. Nothing was saved; run grok-commit setup to try again.", nil}
		}
		fmt.Fprintln(ui.out, "Please type 1 or 2, or press Enter for 1.")
	}
}

// readLine waits for a line or for Ctrl-C, which cancels ctx but doesn't
// interrupt a blocked read.
func (ui setupUI) readLine(ctx context.Context) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := ui.lines.ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && (r.line == "" || !errors.Is(r.err, io.EOF)) {
			fmt.Fprintln(ui.out)
			return "", errSetupCancelled
		}
		return strings.TrimSpace(r.line), nil
	case <-ctx.Done():
		fmt.Fprintln(ui.out)
		return "", errSetupCancelled
	}
}

// enterKey asks again after a typo or a rejected key, up to three times.
func (ui setupUI) enterKey(ctx context.Context, c Config) (string, error) {
	for try := 1; ; try++ {
		key, err := ui.secret()
		if err != nil {
			return "", err
		}
		var problem string
		switch {
		case key == "":
			problem = "No key was entered. Paste your xAI API key, or press Ctrl-C to cancel."
		case len(key) > 8192 || strings.ContainsAny(key, "\r\n\t "):
			problem = "That doesn't look like an API key: keys are one line with no spaces. Please paste it again."
		default:
			probe := c
			probe.Auth, probe.keyOverride = "api", key
			if err := probe.Resolve(); err != nil {
				return "", err
			}
			err := ui.check(ctx, probe)
			if err == nil {
				return key, nil
			}
			var rejected authError
			if errors.Is(err, context.Canceled) {
				return "", err
			}
			if !errors.As(err, &rejected) || rejected.status != 401 || try == 3 {
				return "", fmt.Errorf("%w\nNothing was saved.", err)
			}
			problem = "Grok didn't accept that key. Check that you copied all of it, then paste it again."
		}
		if try == 3 {
			return "", notice{"No valid API key was entered, so nothing was saved. When you have your key, run: grok-commit setup --auth api", nil}
		}
		fmt.Fprintln(ui.out, problem)
	}
}

func (ui setupUI) signIn(ctx context.Context, c Config) (Config, error) {
	if err := ui.login(ctx); err != nil {
		return c, err
	}
	probe := c
	probe.Auth = "cli"
	if err := probe.Resolve(); err != nil {
		return c, err
	}
	return probe, ui.check(ctx, probe)
}
