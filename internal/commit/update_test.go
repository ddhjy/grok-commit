package commit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func updateConfig(t *testing.T) Config {
	t.Helper()
	c := Config{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	t.Setenv("GROK_COMMIT_CONFIG_DIR", c.ConfigDir)
	t.Setenv("GROK_COMMIT_STATE_DIR", c.StateDir)
	t.Setenv("GROK_COMMIT_AUTO_UPDATE", "1")
	return c
}

func tarPackage(t *testing.T, headers []tar.Header, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.WriteString(tw, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func releaseServer(t *testing.T, body string, corrupt bool) distribution {
	t.Helper()
	payload := tarPackage(t, []tar.Header{{Name: "grok-commit", Typeflag: tar.TypeReg, Mode: 0755}}, body)
	name := "grok-commit_0.2.0_linux_amd64.tar.gz"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("release request leaked auth")
		}
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.2.0","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`, name, server.URL+"/archive", server.URL+"/checksums")
		case "/archive":
			_, _ = w.Write(payload)
		case "/checksums":
			hash := digest(payload)
			if corrupt {
				hash = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  %s\n", hash, name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	d := newDistribution()
	d.client = server.Client()
	d.latestURL = server.URL + "/latest"
	d.goos = "linux"
	d.arch = "amd64"
	d.validateAsset = func(raw, _, _ string) bool { return strings.HasPrefix(raw, server.URL+"/") }
	d.verifyBinary = func(_ context.Context, path, version string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(b) != "version="+version {
			return errors.New("bad executable version")
		}
		return nil
	}
	return d
}

func TestUpdateInstallRollbackAndSkip(t *testing.T) {
	c := updateConfig(t)
	target := filepath.Join(t.TempDir(), "grok-commit")
	writeTest(t, target, "version=0.1.0")
	if err := registerInstallation(c, target, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	d := releaseServer(t, "version=0.2.0", false)
	if _, err := performUpdate(context.Background(), c, target, "0.1.0", false, true, d); err != nil {
		t.Fatal(err)
	}
	if err := d.verifyBinary(context.Background(), target, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	state, err := readUpdateState(c, target)
	if err != nil {
		t.Fatal(err)
	}
	if state.Previous != "0.1.0" || state.Installed != "0.2.0" || time.Until(state.NextCheck) < 6*24*time.Hour {
		t.Fatalf("bad state: %+v", state)
	}
	if err := rollbackUpdate(context.Background(), c, target, "0.2.0", d); err != nil {
		t.Fatal(err)
	}
	if err := d.verifyBinary(context.Background(), target, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := performUpdate(context.Background(), c, target, "0.1.0", false, true, d); err != nil {
		t.Fatal(err)
	}
	if err := d.verifyBinary(context.Background(), target, "0.1.0"); err != nil {
		t.Fatal("automatic update reinstalled rolled-back release")
	}
	// An explicit update may retry a version the user previously rolled back.
	if _, err := performUpdate(context.Background(), c, target, "0.1.0", false, false, d); err != nil {
		t.Fatal(err)
	}
	if err := d.verifyBinary(context.Background(), target, "0.2.0"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateFailuresRetainInstallationAndBackOff(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		corrupt    bool
	}{{"checksum", "version=0.2.0", true}, {"wrong version", "version=9.9.9", false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := updateConfig(t)
			target := filepath.Join(t.TempDir(), "grok-commit")
			writeTest(t, target, "version=0.1.0")
			d := releaseServer(t, tc.body, tc.corrupt)
			if _, err := performUpdate(context.Background(), c, target, "0.1.0", false, true, d); err == nil {
				t.Fatal("accepted invalid update")
			}
			if err := d.verifyBinary(context.Background(), target, "0.1.0"); err != nil {
				t.Fatal(err)
			}
			state, _ := readUpdateState(c, target)
			if state.LastError == "" || time.Until(state.NextCheck) < 6*24*time.Hour {
				t.Fatalf("failure not backed off: %+v", state)
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".grok-commit-*"))
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}

func TestUpdateCheckAndOfflineFailure(t *testing.T) {
	c := updateConfig(t)
	target := filepath.Join(t.TempDir(), "grok-commit")
	writeTest(t, target, "version=0.1.0")
	d := releaseServer(t, "version=0.2.0", false)
	message, err := performUpdate(context.Background(), c, target, "0.1.0", true, false, d)
	if err != nil || !strings.Contains(message, "Version 0.2.0 is available (you have 0.1.0)") {
		t.Fatalf("%s: %v", message, err)
	}
	if err = d.verifyBinary(context.Background(), target, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = performUpdate(ctx, c, target, "0.1.0", false, true, d); err == nil {
		t.Fatal("offline update succeeded")
	}
	state, _ := readUpdateState(c, target)
	if time.Until(state.NextCheck) < 6*24*time.Hour {
		t.Fatal("offline check would retry too soon")
	}
}

func TestFailedReplacementPreservesEarlierBackup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "grok-commit")
	writeTest(t, target, "current")
	writeTest(t, previousPath(target), "previous")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := installCandidate(ctx, target+".missing", target); err == nil {
		t.Fatal("missing candidate installed")
	}
	for path, want := range map[string]string{target: "current", previousPath(target): "previous"} {
		got, _ := os.ReadFile(path)
		if string(got) != want {
			t.Fatalf("%s changed after failure", path)
		}
	}
}

func TestFailedHandoffIsNotRetriedDaily(t *testing.T) {
	c := updateConfig(t)
	target := filepath.Join(t.TempDir(), "missing-binary")
	if err := startUpdateWorker(c, target, "auto"); err == nil {
		t.Fatal("missing binary spawned")
	}
	first, _ := readUpdateState(c, target)
	if time.Until(first.NextCheck) < 6*24*time.Hour {
		t.Fatal("failed handoff not backed off")
	}
	if err := startUpdateWorker(c, target, "auto"); err != nil {
		t.Fatalf("retried handoff: %v", err)
	}
	second, _ := readUpdateState(c, target)
	if !first.LastCheck.Equal(second.LastCheck) {
		t.Fatal("repeated handoff")
	}
}

func TestUpdateLockAcrossPlatforms(t *testing.T) {
	c := updateConfig(t)
	target := filepath.Join(t.TempDir(), "grok-commit")
	unlock, err := updateLock(c, target)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, e := updateLock(c, target); e == nil {
		duplicate()
		unlock()
		t.Fatal("parallel updater acquired lock")
	}
	unlock()
	unlock, err = updateLock(c, target)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestUpdateVersionsIntervalsAndTrustedAssets(t *testing.T) {
	for _, s := range []string{"v0.2.0-beta", "v01.2.0", "v1.2", "../v1.2.3", "v18446744073709551616.0.0"} {
		if _, err := versionParts(s); err == nil {
			t.Fatal(s)
		}
	}
	if !newer("v0.10.0", "0.9.9") || newer("v0.2.0", "0.2.0") || newer("v0.1.0", "0.2.0") {
		t.Fatal("version ordering")
	}
	for _, s := range []string{"1h", "0d", "366d", "weekly", "-1d"} {
		if _, err := updateInterval(Config{UpdateInterval: s}); err == nil {
			t.Fatal(s)
		}
	}
	if d, err := updateInterval(Config{}); err != nil || d != 7*24*time.Hour {
		t.Fatal(d, err)
	}
	url := "https://github.com/ddhjy/grok-commit/releases/download/v0.2.0/checksums.txt"
	if !trustedAsset(url, "v0.2.0", "checksums.txt") {
		t.Fatal("official URL rejected")
	}
	for _, bad := range []string{strings.Replace(url, "ddhjy", "someone", 1), url + "?redirect=x", strings.Replace(url, "https:", "http:", 1), strings.Replace(url, "github.com", "github.com.attacker.test", 1)} {
		if trustedAsset(bad, "v0.2.0", "checksums.txt") {
			t.Fatal(bad)
		}
	}
	for _, manifest := range []string{"", "nohash  archive", strings.Repeat("a", 64) + "  archive\n" + strings.Repeat("b", 64) + "  archive"} {
		if _, err := manifestHash(manifest, "archive"); err == nil {
			t.Fatal("bad checksum manifest accepted")
		}
	}
}

func TestArchiveRejectsMissingDuplicateAndSymlink(t *testing.T) {
	for _, headers := range [][]tar.Header{
		{{Name: "../grok-commit", Typeflag: tar.TypeReg}},
		{{Name: "grok-commit", Typeflag: tar.TypeSymlink, Linkname: "/tmp/other"}},
		{{Name: "grok-commit", Typeflag: tar.TypeReg}, {Name: "grok-commit", Typeflag: tar.TypeReg}},
	} {
		if err := extractTar(bytes.NewReader(tarPackage(t, headers, "binary")), "grok-commit", io.Discard); err == nil {
			t.Fatal("invalid tar accepted")
		}
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < 2; i++ {
		f, err := z.Create("grok-commit.exe")
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, "binary")
	}
	z.Close()
	if err := extractZIP(bytes.NewReader(b.Bytes()), int64(b.Len()), "grok-commit.exe", io.Discard); err == nil {
		t.Fatal("duplicate ZIP accepted")
	}
}

func TestUpdatesSummaryNamesTheEnvironmentOverride(t *testing.T) {
	c := updateConfig(t)
	week := 7 * 24 * time.Hour
	if got := updatesSummary(c, week); !strings.HasPrefix(got, "Automatic updates: on (set by GROK_COMMIT_AUTO_UPDATE=1 in this shell).") || strings.Contains(got, "--disable") {
		t.Fatal(got)
	}
	t.Setenv("GROK_COMMIT_AUTO_UPDATE", "0")
	if got := updatesSummary(c, week); got != "Automatic updates: off (set by GROK_COMMIT_AUTO_UPDATE=0 in this shell)." {
		t.Fatal(got)
	}
	os.Unsetenv("GROK_COMMIT_AUTO_UPDATE")
	if got := updatesSummary(c, week); !strings.Contains(got, "checks at most once a week") || !strings.HasSuffix(got, "To turn them off: grok-commit update --disable") {
		t.Fatal(got)
	}
	off := false
	c.AutoUpdate = &off
	if got := updatesSummary(c, week); got != "Automatic updates: off. To turn them on: grok-commit update --enable" {
		t.Fatal(got)
	}
}

func TestUpdateConfigurationAndWeeklyFastPath(t *testing.T) {
	c := updateConfig(t)
	if err := setConfigValue(c, "model", "grok-4.3"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := updateCommand(context.Background(), c, []string{"--disable"}, &output, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(c.ConfigDir, "config.json"))
	var fields map[string]any
	json.Unmarshal(b, &fields)
	if fields["model"] != "grok-4.3" || fields["auto_update"] != false {
		t.Fatalf("%s", b)
	}
	if err := updateCommand(context.Background(), c, []string{"--interval", "14d"}, &output, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	target, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := registerInstallation(c, target, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CI", "")
	before, _ := readUpdateState(c, target)
	start := time.Now()
	for i := 0; i < 100; i++ {
		MaybeAutoUpdate("0.2.0", nil)
	}
	t.Logf("weekly local check: %.3f ms/call", float64(time.Since(start).Microseconds())/100/1000)
	after, _ := readUpdateState(c, target)
	if !after.LastCheck.Equal(before.LastCheck) {
		t.Fatal("updater ran before due date")
	}
	entries, _ := os.ReadDir(updateDir(c, target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "helper-") {
			t.Fatal("helper spawned on fast path")
		}
	}
}
