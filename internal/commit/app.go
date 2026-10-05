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
	"strings"
	"time"
)

const help = `grok-commit — fast, accurate Git commit subjects powered by Grok

Usage: grok-commit [options]
       grok-commit setup [--yes|--auth api|cli]
       grok-commit update [--check|--status|--disable|--enable|--rollback|--interval 7d]
       grok-commit daemon start|status|stop|warm|install|uninstall
       grok-commit auth --stdin
       grok-commit doctor
       grok-commit version

  -a, --all        Stage all changes, including untracked files
  -p, --push       Push after a successful commit
  -s, --history    Match the language/style of recent commits
  -d, --dry-run    Preview staged changes; never stage, commit, or push
      --no-stage   Use the current index without staging
      --no-verify  Explicitly skip Git commit hooks
      --no-cache   Generate a fresh subject without reading/writing cache
      --no-daemon  Connect directly without a background worker
      --profile    Print per-stage timing to stderr
      --model      Grok model (default: grok-4.3)
      --reasoning  none, low, medium, or high (default: none for grok-4.3)
      --auth       auto, api, or cli (default: auto)
      --base-url   API endpoint (API-key mode only)
      --timeout    Generation deadline (default: 30s)
      --hedge-delay  Start one backup request after this delay; 0 disables it
  -h, --help       Show help

Authentication: XAI_API_KEY, a key saved with auth --stdin, or Grok CLI login.
Rules: .grok-commit-rules, or .bunnygit/rules/commit for compatibility.
Git hooks and signing settings are respected. Windows uses direct connections.
`

func Run(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer, version string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Fprintln(out, "grok-commit", version)
			return nil
		case "help", "--help", "-h":
			fmt.Fprint(out, help)
			return nil
		case "auth":
			return saveAuth(args[1:], stdin, out)
		}
	}
	c, err := LoadConfig()
	if err != nil {
		return err
	}
	c.Version = version
	if len(args) > 0 {
		switch args[0] {
		case "setup":
			return setupCommand(ctx, c, args[1:], stdin, out, errOut, version)
		case "update":
			return updateCommand(ctx, c, args[1:], out, version)
		case "__install":
			target, err := executablePath()
			if err != nil {
				return err
			}
			return registerInstallation(c, target, version)
		case "__update":
			return runUpdateWorker(ctx, c, args[1:], version)
		}
	}
	if len(args) > 0 && args[0] == "daemon" {
		return daemonCommand(ctx, c, args[1:], out)
	}
	if len(args) > 0 && args[0] == "doctor" {
		if err := c.Resolve(); err != nil {
			return err
		}
		if _, err := exec.LookPath("git"); err != nil {
			return err
		}
		g, err := NewGrok(ctx, c)
		if err != nil {
			return err
		}
		defer g.Close()
		if err := g.Warm(ctx); err != nil {
			return err
		}
		fmt.Fprintf(out, "Git: available\nGrok authentication: %s\nEndpoint: %s\nModel: %s (%s reasoning)\nConnection: healthy\n", c.Auth, c.BaseURL, c.Model, c.Reasoning)
		return nil
	}
	flags := flag.NewFlagSet("grok-commit", flag.ContinueOnError)
	flags.SetOutput(errOut)
	var all, push, history, dry, noStage, noVerify, noCache, noDaemon, profile, showHelp bool
	flags.BoolVar(&all, "a", false, "")
	flags.BoolVar(&all, "all", false, "")
	flags.BoolVar(&push, "p", false, "")
	flags.BoolVar(&push, "push", false, "")
	flags.BoolVar(&history, "s", false, "")
	flags.BoolVar(&history, "history", false, "")
	flags.BoolVar(&dry, "d", false, "")
	flags.BoolVar(&dry, "dry-run", false, "")
	flags.BoolVar(&showHelp, "h", false, "")
	flags.BoolVar(&showHelp, "help", false, "")
	flags.BoolVar(&noStage, "no-stage", false, "")
	flags.BoolVar(&noVerify, "no-verify", false, "")
	flags.BoolVar(&noCache, "no-cache", false, "")
	flags.BoolVar(&noDaemon, "no-daemon", false, "")
	flags.BoolVar(&profile, "profile", false, "")
	flags.StringVar(&c.Model, "model", c.Model, "")
	flags.StringVar(&c.Reasoning, "reasoning", c.Reasoning, "")
	flags.StringVar(&c.Auth, "auth", c.Auth, "")
	flags.StringVar(&c.BaseURL, "base-url", c.BaseURL, "")
	flags.StringVar(&c.Timeout, "timeout", c.Timeout, "")
	flags.StringVar(&c.HedgeDelay, "hedge-delay", c.HedgeDelay, "")
	flags.Usage = func() { fmt.Fprint(out, help) }
	if err := flags.Parse(expandFlags(args)); err != nil {
		return err
	}
	if showHelp {
		fmt.Fprint(out, help)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unknown argument %q", flags.Arg(0))
	}
	if dry && push {
		return errors.New("--dry-run cannot be combined with --push")
	}
	// First-time interactive use leads to setup before touching the Git index.
	if probe := c; terminalInput(stdin) && probe.Resolve() != nil {
		return setupCommand(ctx, c, nil, stdin, out, errOut, version)
	}
	profile = profile || os.Getenv("GROK_COMMIT_PROFILE") == "1"
	noCache = noCache || os.Getenv("GROK_COMMIT_CACHE") == "0"
	noDaemon = noDaemon || os.Getenv("GROK_COMMIT_DAEMON") == "0"
	start := time.Now()
	last := start
	var timings []string
	mark := func(name string) {
		now := time.Now()
		timings = append(timings, fmt.Sprintf("%s=%.1fms", name, float64(now.Sub(last).Microseconds())/1000))
		last = now
	}
	defer func() {
		if profile {
			fmt.Fprintf(errOut, "[profile] %s total=%.1fms\n", strings.Join(timings, " "), float64(time.Since(start).Microseconds())/1000)
		}
	}()
	r := Repository{}
	status, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status == "" {
		fmt.Fprintln(out, "✓ Working directory clean")
		return nil
	}
	// Check auth/options before making any index changes.
	if err := c.Resolve(); err != nil {
		return err
	}
	mark("preflight")
	fmt.Fprintln(out, strings.TrimRight(status, "\n"))
	if !noStage && !dry {
		stageFlag := "-u"
		if all {
			stageFlag = "-A"
		}
		if _, err := r.git(ctx, "add", stageFlag); err != nil {
			return err
		}
	}
	mark("stage")
	s, err := r.snapshot(ctx, history)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\n"+strings.TrimSpace(s.Stat))
	if s.Truncated {
		fmt.Fprintln(out, "Large diff: using bounded excerpts and the file summary.")
	}
	req := Request{Model: c.Model, Reasoning: c.Reasoning, System: SystemPrompt(s.Rules), Prompt: Prompt(s.Diff, s.History, s.Rules), NoCache: noCache}
	mark("diff")
	fmt.Fprintf(out, "\nGenerating (%s)... ", c.Model)
	result, err := generate(ctx, c, req, noDaemon)
	if err != nil {
		fmt.Fprintln(out, "failed")
		mark("grok")
		return err
	}
	mark("grok")
	fmt.Fprintln(out, "✓")
	fmt.Fprintf(out, "\n→ %s\n", result.Subject)
	if dry {
		fmt.Fprintf(out, "\nPreview in %.3fs\n", time.Since(start).Seconds())
		return nil
	}
	if err := r.commit(ctx, s, result.Subject, noVerify); err != nil {
		return err
	}
	mark("commit")
	fmt.Fprintf(out, "\n✓ Committed in %.3fs\n", time.Since(start).Seconds())
	if push {
		cmd := exec.CommandContext(ctx, "git", "push")
		cmd.Stdin = stdin
		cmd.Stdout = out
		cmd.Stderr = errOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("commit succeeded, but push failed: %w", err)
		}
		mark("push")
	}
	return nil
}

func expandFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "apsdh") == "" {
			for _, r := range a[1:] {
				out = append(out, "-"+string(r))
			}
		} else {
			out = append(out, a)
		}
	}
	return out
}

func saveAuth(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) != 1 || args[0] != "--stdin" {
		return errors.New("use grok-commit auth --stdin; API keys are never accepted as command-line arguments")
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 8193))
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if key == "" || len(b) > 8192 || strings.ContainsAny(key, "\r\n\t ") {
		return errors.New("expected one API key on stdin")
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"api_key": key})
	if err := atomicWrite(filepath.Join(dir, "credentials.json"), data); err != nil {
		return err
	}
	fmt.Fprintln(out, "Saved API key in your private user configuration directory.")
	return nil
}

func daemonCommand(ctx context.Context, c Config, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: grok-commit daemon start|status|stop|warm|install|uninstall")
	}
	if runtime.GOOS == "windows" {
		return errors.New("Windows uses direct requests; a background service is not required")
	}
	// Removing a login service must work even after credentials are removed.
	if args[0] == "uninstall" {
		return service(ctx, c, true)
	}
	if err := c.Resolve(); err != nil {
		return err
	}
	switch args[0] {
	case "run":
		return runDaemon(ctx, c, len(args) == 2 && args[1] == "--keepalive")
	case "status":
		if daemonReady(ctx, c) {
			fmt.Fprintln(out, "Grok connection service is running.")
		} else {
			fmt.Fprintln(out, "Grok connection service is stopped.")
		}
		return nil
	case "start", "warm":
		if err := startDaemon(ctx, c); err != nil {
			return err
		}
		var result Result
		if err := localCall(ctx, c, "/warm", nil, &result); err != nil {
			return err
		}
		if result.Error != "" {
			return errors.New(result.Error)
		}
		fmt.Fprintln(out, "Grok connection service is running and warmed.")
	case "stop":
		if err := stopDaemon(ctx, c); err != nil {
			return err
		}
		fmt.Fprintln(out, "Grok connection service stopped.")
	case "install", "uninstall":
		if err := service(ctx, c, args[0] == "uninstall"); err != nil {
			return err
		}
		fmt.Fprintln(out, "Grok login service", args[0]+"ed.")
	default:
		return fmt.Errorf("unknown daemon action %q", args[0])
	}
	return nil
}
