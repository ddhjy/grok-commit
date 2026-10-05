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
	"slices"
	"strings"
	"time"
)

const help = `grok-commit writes your Git commit subject with Grok, then commits.

Get started:
  grok-commit setup    Connect to Grok (one time, about a minute)
  grok-commit -a       Stage all changes, write the subject, and commit

Usage: grok-commit [options]

By default, grok-commit stages changes to files Git already tracks, asks Grok
for a conventional commit subject, and commits.

Options:
  -a, --all          Also stage new (untracked) files
  -p, --push         Push after committing
  -s, --history      Match the language and style of recent commit subjects
  -d, --dry-run      Show the subject for staged changes without committing
      --no-stage     Commit only what is already staged
      --no-verify    Skip Git commit hooks
      --no-cache     Write a fresh subject instead of reusing a cached one
      --no-daemon    Connect directly instead of through the background service
      --profile      Show how long each step took (on stderr)
      --model        Grok model (default: grok-4.3)
      --reasoning    none, low, medium, or high (default: none for grok-4.3)
      --auth         How to connect: auto, api, or cli (default: auto)
      --base-url     API address, for API keys only (default: xAI's API)
      --timeout      How long to wait for Grok, up to 5m (default: 30s)
      --hedge-delay  If Grok hasn't answered after this long, send one backup
                     request; may use two requests (default: 700ms; 0 = off)
  -h, --help         Show this help

Commands:
  setup              Connect to Grok, or switch how grok-commit connects
  doctor             Check Git and your connection to Grok
  update             Install the latest version or manage automatic updates
  daemon             Manage the background service (macOS and Linux)
  auth --stdin       Save an API key from standard input
  version            Show the installed version
Run grok-commit <command> --help for details.

grok-commit connects with XAI_API_KEY, a saved API key, or your Grok CLI
sign-in. Add project rules in .grok-commit-rules at the repository root.
Git hooks and commit signing work as usual.
`

// commandHelp is printed verbatim; some entries contain a literal '%s'.
var commandHelp = map[string]string{
	"setup": `Usage: grok-commit setup [--auth api|cli] [--yes]

Connects grok-commit to Grok and checks the connection. It never commits.

  --auth api   Connect with an xAI API key (also replaces a saved key)
  --auth cli   Connect with your Grok CLI sign-in (runs grok login)
  --yes        Don't ask anything; just check the current connection
`,
	"update": `Usage: grok-commit update [option]

Without an option, installs the latest version now.

  --check          See whether a new version is available, without installing
  --status         Show your version and automatic-update settings
  --enable         Turn on automatic updates
  --disable        Turn off automatic updates
  --interval 14d   Check every 14 days instead (1d to 365d; default: 7d)
  --rollback       Go back to the version you had before the last update
`,
	"daemon": `Usage: grok-commit daemon <action>    (macOS and Linux)

The background service keeps a warm connection to Grok so commits finish
faster. It starts automatically when you commit and stops after 20 minutes
without use.

  status      Show whether it's running
  start       Start it and connect to Grok now (warm does the same)
  stop        Stop it; your next commit starts it again
  install     Start it each time you log in and keep it running (macOS)
  uninstall   Stop starting it at login (macOS)
`,
	"auth": `Usage: printf '%s' "$XAI_API_KEY" | grok-commit auth --stdin

Saves an xAI API key for future commits. The key is read from standard input,
so it never appears in your shell history. To paste a key instead, run:
grok-commit setup --auth api
`,
	"doctor": `Usage: grok-commit doctor

Checks that Git is installed and that grok-commit can reach Grok with your
credentials. It doesn't write a subject or change any repository.
`,
}

var commands = []string{"setup", "update", "daemon", "auth", "doctor", "version", "help"}

func wantsHelp(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "--help" || a == "-help" })
}

func Run(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer, version string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Fprintln(out, "grok-commit", version)
			return nil
		case "help", "--help", "-h":
			if text := commandHelp[strings.Join(args[1:], "")]; len(args) == 2 && text != "" {
				fmt.Fprint(out, text)
			} else {
				fmt.Fprint(out, help)
			}
			return nil
		}
		if text := commandHelp[args[0]]; text != "" && wantsHelp(args[1:]) {
			fmt.Fprint(out, text)
			return nil
		}
		if args[0] == "auth" {
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
			return setupCommand(ctx, c, args[1:], "", stdin, out, errOut, version)
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
		case "daemon":
			return daemonCommand(ctx, c, args[1:], out)
		case "doctor":
			return doctor(ctx, c, out)
		}
	}
	flags := flag.NewFlagSet("grok-commit", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
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
	if err := flags.Parse(expandFlags(args)); err != nil {
		return flagError(err, "grok-commit", flags, args)
	}
	if showHelp {
		fmt.Fprint(out, help)
		return nil
	}
	if flags.NArg() != 0 {
		return unexpectedArgument(flags.Arg(0))
	}
	if dry && push {
		return errors.New("--dry-run and --push can't be used together: a dry run doesn't commit, so there's nothing to push.")
	}
	// First-time interactive use leads to setup before touching the Git index.
	if probe := c; terminalInput(stdin) && errors.As(probe.Resolve(), new(credentialsError)) {
		return setupCommand(ctx, c, nil, command(args), stdin, out, errOut, version)
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
		fmt.Fprintln(out, "✓ Nothing to commit — working tree clean.")
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
	if errors.Is(err, errNothingStaged) {
		return nothingStaged(status, all, dry, noStage)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\n"+strings.TrimRight(s.Stat, "\n"))
	if s.Truncated {
		fmt.Fprintln(out, "Large diff: Grok will see the file summary and the first part of each file's changes.")
	}
	req := Request{Model: c.Model, Reasoning: c.Reasoning, System: SystemPrompt(s.Rules), Prompt: Prompt(s.Diff, s.History, s.Rules), NoCache: noCache}
	mark("diff")
	fmt.Fprintf(out, "\nWriting the commit subject with %s... ", c.Model)
	result, err := generate(ctx, c, req, noDaemon)
	mark("grok")
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(out)
			return errCommitCancelled
		}
		fmt.Fprintln(out, "failed; nothing was committed")
		return err
	}
	if result.Cached {
		fmt.Fprintln(out, "✓ (cached)")
	} else {
		fmt.Fprintln(out, "✓")
	}
	fmt.Fprintf(out, "\n→ %s\n", result.Subject)
	if dry {
		fmt.Fprintf(out, "\nPreview finished in %.3fs. Nothing was committed.\nTo commit these staged changes, run: %s\n", time.Since(start).Seconds(), commitCommand(args))
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
			if ctx.Err() != nil {
				return notice{"Push cancelled. Your commit is saved locally; run git push when you're ready.", context.Canceled}
			}
			return errors.New("git push failed, but your commit is saved locally (see Git's message above). After fixing the problem, run: git push")
		}
		mark("push")
		fmt.Fprintln(out, "✓ Pushed")
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

func unexpectedArgument(arg string) error {
	if match := closest(arg, commands); match != "" {
		return fmt.Errorf("Unknown command %q. Did you mean grok-commit %s?", arg, match)
	}
	return fmt.Errorf("grok-commit doesn't take arguments like %q; it writes the commit subject itself. To commit only some files, stage them with git add, then run: grok-commit --no-stage", arg)
}

func nothingStaged(status string, all, dry, noStage bool) error {
	untrackedOnly := true
	for _, line := range strings.Split(strings.TrimRight(status, "\n"), "\n") {
		untrackedOnly = untrackedOnly && strings.HasPrefix(line, "?? ")
	}
	var msg string
	switch {
	case dry:
		msg = "Nothing is staged yet, and --dry-run doesn't stage files.\nStage what you want to preview with git add, then run grok-commit --dry-run again."
	case noStage:
		msg = "Nothing is staged yet, and --no-stage only commits what's already staged.\nStage files with git add, or run grok-commit without --no-stage."
	case !all && untrackedOnly:
		msg = "Only new files changed, and new files are included only with -a.\nTo commit them, run: grok-commit -a"
	default:
		msg = "Git didn't stage any of these changes, so there's nothing to commit.\nStage what you want with git add, then run: grok-commit --no-stage"
	}
	return notice{msg, errNothingStaged}
}

// commitCommand turns a dry run's arguments into the command that commits
// exactly the changes it previewed.
func commitCommand(args []string) string {
	var kept []string
	for _, a := range expandFlags(args) {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && slices.Contains([]string{"d", "dry-run", "a", "all", "no-stage"}, name) {
			continue
		}
		kept = append(kept, a)
	}
	return command(append(kept, "--no-stage"))
}

func doctor(ctx context.Context, c Config, out io.Writer) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errGitMissing
	}
	fmt.Fprintln(out, "✓ Git is installed")
	if err := c.Resolve(); err != nil {
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
	fmt.Fprintf(out, "✓ Connected to Grok with your %s\n  Endpoint: %s\n  Model: %s (reasoning: %s)\nEverything looks good.\n", authLabel(c.Auth), c.BaseURL, c.Model, c.Reasoning)
	return nil
}

func saveAuth(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) != 1 || args[0] != "--stdin" {
		msg := "grok-commit auth reads your key from standard input, so it never ends up in your shell history.\nPipe it in: printf '%s' \"$XAI_API_KEY\" | grok-commit auth --stdin\nOr paste it privately with: grok-commit setup --auth api"
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && strings.Trim(a, "abcdefghijklmnopqrstuvwxyz") != "" {
				msg += "\nIf you typed a real key, it may now be in your shell history. Consider replacing it at https://console.x.ai"
				break
			}
		}
		return errors.New(msg)
	}
	if terminalInput(stdin) {
		return errors.New("auth --stdin expects the key to be piped in, for example: printf '%s' \"$XAI_API_KEY\" | grok-commit auth --stdin\nTo paste a key privately instead, run: grok-commit setup --auth api")
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 8193))
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return errors.New("No API key arrived on standard input. Pipe it in, for example: printf '%s' \"$XAI_API_KEY\" | grok-commit auth --stdin")
	}
	if len(b) > 8192 || strings.ContainsAny(key, "\r\n\t ") {
		return errors.New("Standard input should contain only the API key: one line with no spaces. Nothing was saved.")
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"api_key": key})
	if err := atomicWrite(filepath.Join(dir, "credentials.json"), data); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ API key saved in %s (outside your repositories).\n", dir)
	if env := strings.TrimSpace(os.Getenv("XAI_API_KEY")); env != "" && env != key {
		fmt.Fprintln(out, "Note: XAI_API_KEY in this shell takes priority over the saved key. Remove it from your environment to use the saved key.")
	}
	fmt.Fprintln(out, "Check the connection with: grok-commit doctor")
	return nil
}

var daemonActions = []string{"status", "start", "stop", "warm", "install", "uninstall"}

func daemonCommand(ctx context.Context, c Config, args []string, out io.Writer) error {
	if runtime.GOOS == "windows" {
		return notice{"On Windows, grok-commit connects to Grok directly, so there's no background service to manage.", nil}
	}
	if len(args) == 0 {
		return errors.New("Choose what to do with the background service: grok-commit daemon status, start, stop, warm, install, or uninstall")
	}
	action := args[0]
	if action != "run" && !slices.Contains(daemonActions, action) {
		if match := closest(action, daemonActions); match != "" {
			return fmt.Errorf("Unknown action %q. Did you mean grok-commit daemon %s?", action, match)
		}
		return fmt.Errorf("Unknown action %q. Choose status, start, stop, warm, install, or uninstall.", action)
	}
	// Removing a login service must work even after credentials are removed.
	if action == "uninstall" {
		if runtime.GOOS != "darwin" {
			fmt.Fprintln(out, "Starting at login is available only on macOS, so there's nothing to uninstall.")
			return nil
		}
		installed := loginServiceInstalled()
		if err := service(ctx, c, true); err != nil {
			return err
		}
		if installed {
			fmt.Fprintln(out, "✓ The background service will no longer start at login. It still starts on demand when you commit.")
		} else {
			fmt.Fprintln(out, "The background service wasn't set to start at login, so there was nothing to uninstall.")
		}
		return nil
	}
	if err := c.Resolve(); err != nil {
		return err
	}
	switch action {
	case "run":
		return runDaemon(ctx, c, len(args) == 2 && args[1] == "--keepalive")
	case "status":
		switch {
		case daemonReady(ctx, c):
			fmt.Fprintln(out, "Background service: running. It keeps a warm connection to Grok so commits finish faster.")
		case os.Getenv("GROK_COMMIT_DAEMON") == "0":
			fmt.Fprintln(out, "Background service: off for this shell (GROK_COMMIT_DAEMON=0), so commits connect to Grok directly.")
		default:
			fmt.Fprintln(out, "Background service: stopped. It starts automatically with your next commit.")
		}
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
		fmt.Fprintln(out, "✓ Background service is running and connected to Grok.")
	case "stop":
		if err := stopDaemon(ctx, c); err != nil {
			return err
		}
		if loginServiceInstalled() {
			fmt.Fprintln(out, "✓ Background service stopped. Because it's set to start at login, macOS will restart it within a minute. To turn that off, run: grok-commit daemon uninstall")
		} else {
			fmt.Fprintln(out, "✓ Background service stopped. It starts again automatically with your next commit.")
		}
	case "install":
		if err := service(ctx, c, false); err != nil {
			return err
		}
		fmt.Fprintln(out, "✓ Background service installed. It's running now and will start each time you log in.")
	}
	return nil
}
