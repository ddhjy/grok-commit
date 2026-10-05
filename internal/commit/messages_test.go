package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReportLabelsOnlyRealErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
		code int
	}{
		{errors.New("git push failed."), "Error: git push failed.\n", 1},
		{notice{"Only new files changed.", errNothingStaged}, "Only new files changed.\n", 1},
		{errSetupCancelled, errSetupCancelled.message + "\n", 130},
		{fmt.Errorf("generate: %w", context.Canceled), "Cancelled.\n", 130},
	} {
		var out bytes.Buffer
		if code := Report(&out, c.err); code != c.code || out.String() != c.want {
			t.Errorf("Report(%v) = %d %q", c.err, code, out.String())
		}
	}
}

func TestCommitCommandRepeatsDryRunOptions(t *testing.T) {
	for want, args := range map[string][]string{
		"grok-commit --no-stage":                                 {"-ad"},
		"grok-commit --model grok-4.3 --no-stage":                {"-ad", "--model", "grok-4.3"},
		"grok-commit -s --reasoning=low --no-stage":              {"--dry-run", "-s", "--reasoning=low", "--all", "--no-stage"},
		"grok-commit --base-url 'https://x.test/a b' --no-stage": {"-d", "--base-url", "https://x.test/a b"},
	} {
		if got := commitCommand(args); got != want {
			t.Errorf("commitCommand(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestClosestSuggestsOnlyClearMatches(t *testing.T) {
	names := []string{"push", "profile", "no-stage", "no-verify", "no-cache", "no-daemon", "model"}
	for typed, want := range map[string]string{"pussh": "push", "pus": "push", "modle": "model", "no-cach": "no-cache", "no-": "", "no": "", "xyz": ""} {
		if got := closest(typed, names); got != want {
			t.Errorf("closest(%q) = %q, want %q", typed, got, want)
		}
	}
}

func TestDurationsReadNaturally(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Minute: "1m", 90 * time.Second: "1m30s", 2 * time.Hour: "2h", 400 * time.Millisecond: "400ms"} {
		if got := duration(d); got != want {
			t.Errorf("duration(%v) = %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{24 * time.Hour: "at most once a day", 7 * 24 * time.Hour: "at most once a week", 14 * 24 * time.Hour: "at most once every 14 days"} {
		if got := frequency(d); got != want {
			t.Errorf("frequency(%v) = %q, want %q", d, got, want)
		}
	}
}
