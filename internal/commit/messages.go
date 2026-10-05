package commit

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// notice is an expected outcome that needs the user's attention but isn't a
// malfunction, such as having nothing to commit. Report prints it without an
// "Error:" label.
type notice struct {
	message string
	err     error
}

func (n notice) Error() string { return n.message }
func (n notice) Unwrap() error { return n.err }

// credentialsError means no usable credentials are available yet. Interactive
// commits answer it by starting setup.
type credentialsError string

func (e credentialsError) Error() string { return string(e) }

const (
	errNotConnected = credentialsError("grok-commit isn't connected to Grok yet. Run grok-commit setup in a terminal, or set XAI_API_KEY.")
	errNoCLISignIn  = credentialsError("Grok CLI isn't signed in on this computer. Run grok login, or connect with an API key: grok-commit setup --auth api")
	errDamagedKey   = credentialsError("Your saved API key file is damaged. Save the key again with: grok-commit setup --auth api")
)

var (
	errGitMissing      = errors.New("Git isn't installed (or isn't on your PATH). Install it from https://git-scm.com/downloads, then try again.")
	errNotRepository   = errors.New("This folder isn't part of a Git repository. Run grok-commit from inside your project, or create a repository here with: git init")
	errNoGrokCLI       = errors.New("Grok CLI isn't installed on this computer. Install it and run grok login, or connect with an API key: grok-commit setup --auth api")
	errSetupCancelled  = notice{"Setup cancelled. Nothing was saved; run grok-commit setup whenever you're ready.", context.Canceled}
	errCommitCancelled = notice{"Cancelled. Nothing was committed.", context.Canceled}
)

// Report prints err for the user and returns the process exit status.
func Report(w io.Writer, err error) int {
	code := 1
	if errors.Is(err, context.Canceled) {
		code = 130
	}
	var n notice
	switch {
	case errors.As(err, &n):
		fmt.Fprintln(w, n.message)
	case code == 130:
		fmt.Fprintln(w, "Cancelled.")
	default:
		fmt.Fprintln(w, "Error:", err)
	}
	return code
}

func authLabel(auth string) string {
	if auth == "cli" {
		return "Grok CLI sign-in"
	}
	return "xAI API key"
}

func days(d time.Duration) string {
	if n := d.Hours() / 24; n != 1 {
		return fmt.Sprintf("%g days", n)
	}
	return "1 day"
}

func frequency(d time.Duration) string {
	switch d {
	case 24 * time.Hour:
		return "at most once a day"
	case 7 * 24 * time.Hour:
		return "at most once a week"
	}
	return "at most once every " + days(d)
}

// duration prints 1m instead of Go's 1m0s.
func duration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func preview(s string) string {
	if utf8.RuneCountInString(s) > 60 {
		s = string([]rune(s)[:60]) + "…"
	}
	return strconv.Quote(s)
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

// command formats args as a grok-commit command line that can be pasted into
// a POSIX shell.
func command(args []string) string {
	parts := []string{"grok-commit"}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

var flagExamples = map[string]string{"model": "grok-4.3", "reasoning": "low", "auth": "api",
	"base-url": "https://api.x.ai/v1", "timeout": "60s", "hedge-delay": "0", "interval": "14d"}

// flagError rewrites errors from the flag package, which names options with a
// single dash and doesn't suggest a fix.
func flagError(err error, cmd string, fs *flag.FlagSet, args []string) error {
	msg := err.Error()
	more := "Run " + cmd + " --help to see all options."
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		if cmd == "grok-commit" && (name == "message" || strings.Contains(name, "m") && strings.Trim(name, "apsdhm") == "") {
			return errors.New("grok-commit writes the subject for you, so it has no -m option. To write your own message, use git commit -m instead.")
		}
		var names []string
		fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
		if match := closest(name, names); match != "" {
			return fmt.Errorf("Unknown option %s. Did you mean %s? %s", typedOption(name, args), optionName(match), more)
		}
		return fmt.Errorf("Unknown option %s. %s", typedOption(name, args), more)
	}
	if name, ok := strings.CutPrefix(msg, "flag needs an argument: -"); ok {
		if example := flagExamples[name]; example != "" {
			return fmt.Errorf("Option %s needs a value, for example: %s %s", optionName(name), optionName(name), example)
		}
		return fmt.Errorf("Option %s needs a value. %s", optionName(name), more)
	}
	if rest, ok := strings.CutPrefix(msg, "invalid boolean value "); ok {
		if _, name, found := strings.Cut(rest, " for -"); found {
			name, _, _ = strings.Cut(name, ":")
			return fmt.Errorf("Option %s doesn't take a value; use %s on its own.", optionName(name), optionName(name))
		}
	}
	if syntax, ok := strings.CutPrefix(msg, "bad flag syntax: "); ok {
		return fmt.Errorf("Unknown option %s. %s", syntax, more)
	}
	return fmt.Errorf("%v. %s", err, more)
}

func optionName(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// typedOption recovers the option as typed; the flag package strips its dashes.
func typedOption(name string, args []string) string {
	for _, a := range args {
		a, _, _ = strings.Cut(a, "=")
		if a == "-"+name || a == "--"+name {
			return a
		}
	}
	return optionName(name)
}

// closest suggests a correction for a mistyped name: the only name it is a
// prefix of, or the single nearest name within a small edit distance.
func closest(name string, names []string) string {
	if len(name) >= 3 {
		var prefixed []string
		for _, n := range names {
			if strings.HasPrefix(n, name) {
				prefixed = append(prefixed, n)
			}
		}
		if len(prefixed) == 1 {
			return prefixed[0]
		}
	}
	best, bestDistance, tie := "", max(1, len(name)/3)+1, false
	for _, n := range names {
		switch d := editDistance(name, n); {
		case d < bestDistance:
			best, bestDistance, tie = n, d, false
		case d == bestDistance:
			tie = true
		}
	}
	if tie {
		return ""
	}
	return best
}

// editDistance counts insertions, deletions, substitutions and adjacent
// transpositions.
func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	d := make([][]int, len(x)+1)
	for i := range d {
		d[i] = make([]int, len(y)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(x); i++ {
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(x)][len(y)]
}
