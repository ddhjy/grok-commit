package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Repository struct{ Dir string }

var errNothingStaged = errors.New("There are no staged changes to commit.")

func (r Repository) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	if args[0] == "status" {
		// English output lets gitError recognize "not a git repository". Never
		// set this for commit: hooks inherit the environment.
		cmd.Env = append(os.Environ(), "LC_ALL=C")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", gitError(args[0], stderr.String(), err)
	}
	return string(b), nil
}

func gitError(command, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errGitMissing
	case strings.Contains(msg, "not a git repository"):
		return errNotRepository
	case msg == "":
		return fmt.Errorf("git %s failed (%v).", command, err)
	}
	return fmt.Errorf("git %s failed:\n%s", command, indent(msg))
}

type Snapshot struct {
	Tree, Diff, Stat, History, Rules string
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
		return s, errNothingStaged
	}
	s.Stat, err = r.git(ctx, "--no-pager", "diff", "--no-ext-diff", "--no-color", "--stat", head, s.Tree, "--")
	if err != nil {
		return s, err
	}
	if len(s.Diff) > 50000 {
		s.Diff = compactDiff(s.Diff, s.Stat)
	}
	if history {
		s.History, _ = r.git(ctx, "log", "-10", "--format=%s")
	}
	root, err := r.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return s, err
	}
	for _, name := range []string{".grok-commit-rules", filepath.Join(".bunnygit", "rules", "commit")} {
		b, e := readRules(strings.TrimSpace(root), name)
		if e == nil {
			s.Rules = string(b)
			break
		}
		if !errors.Is(e, os.ErrNotExist) {
			return s, e
		}
	}
	return s, nil
}

func readRules(root, name string) ([]byte, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("grok-commit can't use %s because it points outside this repository. Replace it with a regular file inside the repository.", name)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("grok-commit can't use %s because it isn't a regular file. Replace it with a plain text file.", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, 32769))
	if len(b) > 32768 {
		return nil, fmt.Errorf("%s is larger than 32 KiB. Shorten it to the rules that matter most for commit subjects.", name)
	}
	return b, err
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
		return errors.New("Your staged changes were modified while Grok was writing the subject, so nothing was committed. Run grok-commit again to describe the current changes.")
	}
	args := []string{"commit", "--quiet", "-m", subject}
	if noVerify {
		args = append(args, "--no-verify")
	}
	if _, err = r.git(ctx, args...); err != nil {
		if ctx.Err() != nil {
			return errCommitCancelled
		}
		hint := "Fix the problem above, then run grok-commit again."
		if !noVerify && r.hasCommitHooks(ctx) {
			hint += " To skip commit hooks this time, add --no-verify."
		}
		return fmt.Errorf("%w\nNothing was committed, and your changes are still staged. %s", err, hint)
	}
	return nil
}

func (r Repository) hasCommitHooks(ctx context.Context) bool {
	dir, err := r.git(ctx, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return false
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(r.Dir, dir)
	}
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg"} {
		// Outside Windows, Git skips hooks that aren't executable.
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
			return true
		}
	}
	return false
}
