package commit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s (%v)", args, b, err)
	}
	return strings.TrimSpace(string(b))
}

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	for k, v := range map[string]string{"user.name": "Test User", "user.email": "test@example.invalid", "commit.gpgsign": "false", "core.autocrlf": "false", "core.hooksPath": filepath.Join(dir, "hooks")} {
		gitTest(t, dir, "config", k, v)
	}
	writeTest(t, filepath.Join(dir, "code.txt"), "old\n")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-qm", "feat: initial")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	t.Setenv("XAI_API_KEY", "test-key")
	t.Setenv("GROK_COMMIT_AUTH", "api")
	t.Setenv("GROK_COMMIT_CONFIG_DIR", t.TempDir())
	t.Setenv("GROK_COMMIT_STATE_DIR", t.TempDir())
	t.Setenv("GROK_COMMIT_DAEMON", "0")
	t.Setenv("GROK_COMMIT_HEDGE_DELAY", "0")
	return dir
}

func writeTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func runTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), args, strings.NewReader(""), &out, &out, "test")
	return out.String(), err
}

func useServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	t.Setenv("GROK_COMMIT_BASE_URL", s.URL)
}

func TestRealCommitAndHistoryRules(t *testing.T) {
	dir := setupRepo(t)
	writeTest(t, filepath.Join(dir, "code.txt"), "new behavior\n")
	writeTest(t, filepath.Join(dir, ".grok-commit-rules"), "Use Chinese subjects.")
	useServer(t, func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		for _, fragment := range []string{"Use Chinese subjects.", "feat: initial", "new behavior"} {
			if !strings.Contains(b.String(), fragment) {
				t.Errorf("prompt missing %q", fragment)
			}
		}
		sse(w, "feat: 支持新的行为", "stop")
	})
	out, err := runTest(t, "-as", "--profile")
	if err != nil {
		t.Fatal(err, out)
	}
	if got := gitTest(t, dir, "log", "-1", "--format=%s"); got != "feat: 支持新的行为" {
		t.Fatal(got)
	}
	if gitTest(t, dir, "status", "--porcelain") != "" {
		t.Fatal("repository not clean")
	}
	if !strings.Contains(out, "[profile]") {
		t.Fatal("missing profile")
	}
}

func TestDryRunNeverChangesIndexOrHEAD(t *testing.T) {
	dir := setupRepo(t)
	writeTest(t, filepath.Join(dir, "code.txt"), "staged\n")
	gitTest(t, dir, "add", "code.txt")
	writeTest(t, filepath.Join(dir, "new file.txt"), "untracked\n")
	index := gitTest(t, dir, "write-tree")
	head := gitTest(t, dir, "rev-parse", "HEAD")
	useServer(t, func(w http.ResponseWriter, r *http.Request) { sse(w, "feat: preview staged change", "stop") })
	if _, err := runTest(t, "-ad"); err != nil {
		t.Fatal(err)
	}
	if gitTest(t, dir, "write-tree") != index || gitTest(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("dry run mutated repository")
	}
}

func TestFailedGenerationDoesNotCommit(t *testing.T) {
	dir := setupRepo(t)
	writeTest(t, filepath.Join(dir, "code.txt"), "new\n")
	head := gitTest(t, dir, "rev-parse", "HEAD")
	useServer(t, func(w http.ResponseWriter, r *http.Request) { sse(w, "feat: truncated", "length") })
	if _, err := runTest(t, "-a"); err == nil {
		t.Fatal("expected failure")
	}
	if gitTest(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("committed on failure")
	}
}

func TestChangedIndexBlocksCommit(t *testing.T) {
	dir := setupRepo(t)
	writeTest(t, filepath.Join(dir, "code.txt"), "new\n")
	head := gitTest(t, dir, "rev-parse", "HEAD")
	useServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile(filepath.Join(dir, "code.txt"), []byte("different change\n"), 0600)
		cmd := exec.Command("git", "add", "code.txt")
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Error(err)
		}
		sse(w, "feat: original change", "stop")
	})
	if _, err := runTest(t, "-a"); err == nil || !strings.Contains(err.Error(), "staged changes changed") {
		t.Fatalf("wrong error: %v", err)
	}
	if gitTest(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("committed a changed index")
	}
}

func TestCommitHooksAreRespected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook fixture")
	}
	dir := setupRepo(t)
	writeTest(t, filepath.Join(dir, "code.txt"), "new\n")
	if err := os.Mkdir(filepath.Join(dir, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "hooks", "pre-commit")
	writeTest(t, hook, "#!/bin/sh\nexit 1\n")
	_ = os.Chmod(hook, 0700)
	useServer(t, func(w http.ResponseWriter, r *http.Request) { sse(w, "feat: new behavior", "stop") })
	if _, err := runTest(t, "-a"); err == nil {
		t.Fatal("hook was bypassed")
	}
	if _, err := runTest(t, "-a", "--no-verify"); err != nil {
		t.Fatal(err)
	}
}

func TestUnbornRepositoryAndUnusualFilename(t *testing.T) {
	dir := setupRepo(t)
	gitTest(t, dir, "checkout", "--orphan", "new-history")
	name := "file with spaces.txt"
	if runtime.GOOS != "windows" {
		name = "file with\na newline.txt"
	}
	writeTest(t, filepath.Join(dir, name), "new content\n")
	useServer(t, func(w http.ResponseWriter, r *http.Request) { sse(w, "feat: initial files", "stop") })
	if _, err := runTest(t, "-a"); err != nil {
		t.Fatal(err)
	}
	if gitTest(t, dir, "rev-list", "--count", "HEAD") != "1" {
		t.Fatal("expected initial commit")
	}
}

func TestCacheIncludesFullInputAndEndpoint(t *testing.T) {
	c := testConfig(t, "https://api.x.ai/v1")
	r := Request{Model: c.Model, Reasoning: c.Reasoning, System: "rules", Prompt: "diff"}
	b, _ := json.Marshal(cacheEntry{"feat: saved result", time.Now()})
	if err := atomicWrite(cachePath(c, r), b); err != nil {
		t.Fatal(err)
	}
	if result, ok := cached(c, r); !ok || !result.Cached {
		t.Fatal("cache miss")
	}
	for _, change := range []func(*Request){func(r *Request) { r.Prompt += "changed" }, func(r *Request) { r.System += "changed" }, func(r *Request) { r.Model = "grok-4.7" }, func(r *Request) { r.Reasoning = "low" }, func(r *Request) { r.NoCache = true }} {
		other := r
		change(&other)
		if _, ok := cached(c, other); ok {
			t.Fatal("cache failed to invalidate")
		}
	}
	other := c
	other.BaseURL = "https://other.example/v1"
	if _, ok := cached(other, r); ok {
		t.Fatal("cache crossed endpoints")
	}
	b, _ = json.Marshal(cacheEntry{"feat: old result", time.Now().Add(-8 * 24 * time.Hour)})
	_ = atomicWrite(cachePath(c, r), b)
	if _, ok := cached(c, r); ok {
		t.Fatal("expired cache accepted")
	}
}

func TestConfigRejectsUnsafeCredentialEndpoint(t *testing.T) {
	c := testConfig(t, "https://api.x.ai/v1")
	for _, base := range []string{"http://example.com/v1", "https://user:pass@example.com/v1", "https://example.com/v1?key=secret"} {
		other := c
		other.BaseURL = base
		if err := other.Resolve(); err == nil {
			t.Errorf("accepted %q", base)
		}
	}
	c.Auth = "cli"
	c.BaseURL = "https://example.com/v1"
	if err := c.Resolve(); err == nil {
		t.Fatal("CLI credential destination unrestricted")
	}
}

func TestAuthStoresOnlyInPrivateUserConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GROK_COMMIT_CONFIG_DIR", dir)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"auth", "--stdin"}, strings.NewReader("test-secret-key\n"), &out, &out, "test"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "test-secret-key") {
		t.Fatal("key printed")
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("credentials are not private")
	}
}

func TestLargeDiffIsBoundedAndLabelled(t *testing.T) {
	input := "diff --git a/one b/one\n" + strings.Repeat("+content\n", 100) + "diff --git a/two b/two\n+second file\n"
	got := compactDiff(input, strings.Repeat("stat", 20000))
	if len(got) > 50000 || !strings.Contains(got, "truncated") || !strings.Contains(got, "second file") {
		t.Fatal("invalid compact diff")
	}
}

func ExamplePrompt() {
	fmt.Println(strings.Contains(Prompt("+hello", "", "Use Chinese"), "Use Chinese")) // Output: true
}
