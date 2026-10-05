package commit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSetupVerifiesBeforeSavingAndNeverEchoesKey(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := updateConfig(t)
			t.Setenv("XAI_API_KEY", "")
			t.Setenv("GROK_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/models" {
					t.Error("setup generated a model response")
				}
				if r.Header.Get("Authorization") != "Bearer secret-test-key" {
					t.Error("missing key")
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"data":[]}`)
			}))
			defer server.Close()
			c.Model = "grok-4.3"
			c.Auth = "api"
			c.Timeout = "5s"
			c.HedgeDelay = "0"
			c.BaseURL = server.URL
			var out bytes.Buffer
			ui := setupUI{in: strings.NewReader("1\n"), out: &out, interactive: true, secret: func() (string, error) { return "secret-test-key", nil }}
			err := configureAuth(context.Background(), c, ui)
			if strings.Contains(out.String(), "secret-test-key") {
				t.Fatal("key echoed")
			}
			b, readErr := os.ReadFile(filepath.Join(c.ConfigDir, "credentials.json"))
			if status == 401 {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatal("rejected key persisted")
				}
				return
			}
			if err != nil || readErr != nil {
				t.Fatal(err, readErr)
			}
			var saved map[string]string
			if json.Unmarshal(b, &saved) != nil || saved["api_key"] != "secret-test-key" {
				t.Fatal("key not saved")
			}
			info, _ := os.Stat(filepath.Join(c.ConfigDir, "credentials.json"))
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("credentials not private")
			}
		})
	}
}

func TestNoninteractiveSetupDoesNotPrompt(t *testing.T) {
	c := updateConfig(t)
	c.Model = "grok-4.3"
	c.Auth = "api"
	c.Timeout = "5s"
	c.HedgeDelay = "0"
	t.Setenv("XAI_API_KEY", "")
	var out bytes.Buffer
	err := configureAuth(context.Background(), c, setupUI{in: strings.NewReader(""), out: &out})
	if err == nil || !strings.Contains(err.Error(), "setup") || strings.Contains(out.String(), "Choose") {
		t.Fatal(err, out.String())
	}
	// Not being connected yet is expected right after installing.
	var report bytes.Buffer
	if code := Report(&report, err); code != 1 || strings.HasPrefix(report.String(), "Error:") || !errors.As(err, new(credentialsError)) {
		t.Fatal(code, report.String())
	}
}

func TestSetupReusesExistingCredentials(t *testing.T) {
	c := updateConfig(t)
	t.Setenv("XAI_API_KEY", "existing-test-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer existing-test-key" {
			t.Error("unexpected setup request")
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()
	c.Model = "grok-4.3"
	c.Auth = "auto"
	c.Timeout = "5s"
	c.HedgeDelay = "0"
	c.BaseURL = server.URL
	var out bytes.Buffer
	if err := configureAuth(context.Background(), c, setupUI{out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Using the xAI API key in XAI_API_KEY") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("copied environment key into file")
	}
}

func TestSetupAsksAgainAfterRejectedKey(t *testing.T) {
	c := updateConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer right-key" {
			w.WriteHeader(401)
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()
	c.Model, c.Auth, c.Timeout, c.HedgeDelay, c.BaseURL = "grok-4.3", "api", "5s", "0", server.URL
	keys := []string{"wrong-key", "two words", "right-key"}
	var out bytes.Buffer
	ui := setupUI{in: strings.NewReader("\n"), out: &out, interactive: true, secret: func() (string, error) {
		key := keys[0]
		keys = keys[1:]
		return key, nil
	}}
	if err := configureAuth(context.Background(), c, ui); err != nil {
		t.Fatal(err, out.String())
	}
	for _, want := range []string{"Grok didn't accept that key. Check that you copied all of it", "That doesn't look like an API key", "Your API key is saved in"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "wrong-key") || strings.Contains(out.String(), "right-key") {
		t.Fatal("key echoed")
	}
	b, _ := os.ReadFile(filepath.Join(c.ConfigDir, "credentials.json"))
	if !strings.Contains(string(b), "right-key") {
		t.Fatal("accepted key not saved")
	}
}

func TestSetupMenuReasksThenCancelsCleanly(t *testing.T) {
	c := updateConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_HOME", t.TempDir())
	c.Model, c.Auth, c.Timeout, c.HedgeDelay = "grok-4.3", "auto", "5s", "0"
	var out bytes.Buffer
	ui := setupUI{in: strings.NewReader("3\n"), out: &out, interactive: true, retry: "grok-commit -a", secret: func() (string, error) {
		t.Error("asked for a key")
		return "", nil
	}}
	err := configureAuth(context.Background(), c, ui)
	var report bytes.Buffer
	if Report(&report, err) != 130 || !strings.HasPrefix(report.String(), "Setup cancelled. Nothing was saved") {
		t.Fatalf("%v %q", err, report.String())
	}
	for _, want := range []string{"nothing has been staged or committed yet", "1) xAI API key", "Please type 1 or 2, or press Enter for 1."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("saved credentials after cancelling")
	}
}

func TestSetupMenuStopsOnCtrlC(t *testing.T) {
	c := updateConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_HOME", t.TempDir())
	c.Model, c.Auth, c.Timeout, c.HedgeDelay = "grok-4.3", "auto", "5s", "0"
	in, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { done <- configureAuth(ctx, c, setupUI{in: in, out: io.Discard, interactive: true}) }()
	select {
	case err := <-done:
		if !errors.Is(err, errSetupCancelled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("setup ignored Ctrl-C while waiting for input")
	}
}
