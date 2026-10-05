package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repository struct{ Dir string }

func (r Repository) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return string(b), nil
}

type Snapshot struct {
	Tree, Diff, Stat, History, Rules string
	Truncated                        bool
}

func (r Repository) snapshot(ctx context.Context, history bool) (Snapshot, error) {
	var s Snapshot
	var err error
	s.Tree, err = r.git(ctx, "write-tree")
	if err != nil {
		return s, err
	}
	s.Tree = strings.TrimSpace(s.Tree)
	// Diff the captured tree rather than the live index. This also works on an unborn branch.
	head, err := r.git(ctx, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		head, err = r.git(ctx, "hash-object", "-w", "-t", "tree", "--stdin")
		if err != nil {
			return s, err
		}
	}
	head = strings.TrimSpace(head)
	s.Diff, err = r.git(ctx, "--no-pager", "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--unified=3", head, s.Tree, "--")
	if err != nil {
		return s, err
	}
	if s.Diff == "" {
		return s, errors.New("no staged changes")
	}
	s.Stat, err = r.git(ctx, "--no-pager", "diff", "--no-ext-diff", "--no-color", "--stat", head, s.Tree, "--")
	if err != nil {
		return s, err
	}
	if len(s.Diff) > 50000 {
		s.Diff = compactDiff(s.Diff, s.Stat)
		s.Truncated = true
	}
	if history {
		s.History, _ = r.git(ctx, "log", "-10", "--format=%s")
	}
	root, err := r.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return s, err
	}
	for _, name := range []string{".grok-commit-rules", filepath.Join(".bunnygit", "rules", "commit")} {
		b, e := os.ReadFile(filepath.Join(strings.TrimSpace(root), name))
		if e == nil {
			if len(b) > 32768 {
				return s, errors.New("commit rules exceed 32 KiB")
			}
			s.Rules = string(b)
			break
		}
		if !errors.Is(e, os.ErrNotExist) {
			return s, e
		}
	}
	return s, nil
}

func compactDiff(diff, stat string) string {
	var b strings.Builder
	b.WriteString("Large diff: excerpts are truncated. File summary:\n")
	if len(stat) > 16000 {
		stat = stat[:16000]
	}
	b.WriteString(stat)
	lines := 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			lines = 0
		}
		if lines < 50 {
			if b.Len()+len(line)+1 > 50000 {
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		lines++
	}
	return b.String()
}

func (r Repository) commit(ctx context.Context, snapshot Snapshot, subject string, noVerify bool) error {
	tree, err := r.git(ctx, "write-tree")
	if err != nil {
		return err
	}
	if strings.TrimSpace(tree) != snapshot.Tree {
		return errors.New("staged changes changed during generation; run again")
	}
	args := []string{"commit", "--quiet", "-m", subject}
	if noVerify {
		args = append(args, "--no-verify")
	}
	_, err = r.git(ctx, args...)
	return err
}
