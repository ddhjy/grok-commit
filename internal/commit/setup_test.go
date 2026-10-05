package commit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	if !strings.Contains(out.String(), "Reusing existing") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(c.ConfigDir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("copied environment key into file")
	}
}
