package commit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sse(w http.ResponseWriter, subject, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": subject}, "finish_reason": finish}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}

func testConfig(t *testing.T, base string) Config {
	t.Helper()
	t.Setenv("XAI_API_KEY", "test-key")
	c := Config{Model: "grok-4.3", Reasoning: "none", Auth: "api", BaseURL: base, Timeout: "2s", HedgeDelay: "0", StateDir: t.TempDir(), ConfigDir: t.TempDir()}
	if err := c.Resolve(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSubjectValidation(t *testing.T) {
	for _, s := range []string{"feat: support aliases", "fix(config)!: 支持候选路径", "```text\nfeat: add aliases\n```"} {
		if _, err := ValidateSubject(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "Here is your commit", "feat: ", "feat: one\nfix: two", "fix: bad\x1btext", "feat: " + strings.Repeat("长", 70), "fix: a\u2028b"} {
		if _, err := ValidateSubject(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestProjectRulesOverrideDefaultLanguage(t *testing.T) {
	rules := "Use Chinese. Use feat(config): as the prefix."
	if !strings.Contains(SystemPrompt(rules), rules) {
		t.Fatal("rules missing from system instructions")
	}
	if strings.Contains(Prompt("diff", "", rules), "Use English.") {
		t.Fatal("conflicting default language")
	}
	if !strings.Contains(Prompt("diff", "", ""), "Use English.") {
		t.Fatal("default language missing")
	}
}

func TestStreamRequiresSuccessfulCompletion(t *testing.T) {
	cases := map[string]string{
		"partial":      `data: {"choices":[{"delta":{"content":"feat: partial"}}]}`,
		"truncated":    `data: {"choices":[{"delta":{"content":"feat: partial"},"finish_reason":"length"}]}`,
		"refusal":      `data: {"choices":[{"delta":{"refusal":"no"},"finish_reason":"stop"}]}`,
		"tools":        `data: {"choices":[{"delta":{"tool_calls":[{}]},"finish_reason":"stop"}]}`,
		"api_error":    `data: {"error":{"message":"private error"}}`,
		"invalid_json": "data: {broken}",
		"done_only":    "data: [DONE]",
		"bad_format":   `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStream(strings.NewReader(input + "\n\n")); err == nil {
				t.Fatal("accepted invalid stream")
			}
		})
	}
	good := "data: {\"choices\":[{\"delta\":{\"content\":\"feat: good\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"
	if s, err := parseStream(strings.NewReader(good)); err != nil || s != "feat: good" {
		t.Fatalf("%q %v", s, err)
	}
	post := strings.Replace(good, "data: [DONE]", `data: {"choices":[{"delta":{"content":"extra"},"finish_reason":"stop"}]}`, 1)
	if _, err := parseStream(strings.NewReader(post)); err == nil {
		t.Fatal("accepted output after stop")
	}
}

func TestGrokProtocolAndConnectionReuse(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing auth")
		}
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["reasoning_effort"] != "none" || payload["store"] != false || payload["model"] != "grok-4.3" {
			t.Errorf("bad payload settings: %v", payload)
		}
		sse(w, "feat: verified result", "stop")
	}))
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	c := testConfig(t, server.URL)
	g, err := NewGrok(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := Request{Model: c.Model, Reasoning: c.Reasoning, System: systemPrompt, Prompt: "diff"}
	if s, err := g.Generate(context.Background(), r); err != nil || s != "feat: verified result" {
		t.Fatalf("%q %v", s, err)
	}
	if err := g.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 1 {
		t.Errorf("expected one reused connection, got %d", connections.Load())
	}
}

func TestHedgeReturnsFirstValidSubject(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			select {
			case <-time.After(300 * time.Millisecond):
				sse(w, "invalid subject", "stop")
			case <-r.Context().Done():
			}
			return
		}
		sse(w, "fix: use valid backup", "stop")
	}))
	defer server.Close()
	c := testConfig(t, server.URL)
	c.Hedge = 20 * time.Millisecond
	g, _ := NewGrok(context.Background(), c)
	defer g.Close()
	start := time.Now()
	s, err := g.Generate(context.Background(), Request{Model: c.Model, Reasoning: c.Reasoning, System: systemPrompt, Prompt: "diff"})
	if err != nil || s != "fix: use valid backup" {
		t.Fatalf("%q %v", s, err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("waited for the slow primary")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestFastResponseDoesNotHedge(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); sse(w, "fix: fast subject", "stop") }))
	defer server.Close()
	c := testConfig(t, server.URL)
	c.Hedge = time.Second
	g, _ := NewGrok(context.Background(), c)
	defer g.Close()
	if _, err := g.Generate(context.Background(), Request{Model: c.Model}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestTimeoutCancelsRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	c := testConfig(t, server.URL)
	c.RequestTimeout = 30 * time.Millisecond
	c.Hedge = time.Second
	g, _ := NewGrok(context.Background(), c)
	defer g.Close()
	start := time.Now()
	if _, err := g.Generate(context.Background(), Request{Model: c.Model}); err == nil {
		t.Fatal("expected deadline error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline not respected")
	}
}

func TestCredentialsNeverFollowRedirect(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	c := testConfig(t, server.URL)
	g, _ := NewGrok(context.Background(), c)
	defer g.Close()
	if _, err := g.Generate(context.Background(), Request{Model: c.Model}); err == nil {
		t.Fatal("expected redirect error")
	}
	if leaked.Load() != 0 {
		t.Fatal("credential-bearing redirect followed")
	}
}

func TestStatusErrorsSayWhatToDo(t *testing.T) {
	api := &Grok{config: Config{Auth: "api", BaseURL: apiURL, Model: "grok-4.3", Reasoning: "none"}}
	cli := &Grok{config: Config{Auth: "cli", BaseURL: cliURL, Model: "grok-4.3", Reasoning: "none"}}
	for _, c := range []struct {
		g        *Grok
		code     int
		endpoint string
		want     string
	}{
		{api, 401, "/models", "Grok didn't accept your API key (HTTP 401)."},
		{cli, 401, "/chat/completions", "Run grok login, then try again."},
		{api, 403, "/chat/completions", "out of credits"},
		{cli, 426, "/chat/completions", "Grok needs a newer Grok CLI."},
		{api, 429, "/chat/completions", "Wait a minute, then try again."},
		{api, 307, "/models", "doesn't follow redirects, so your key is never sent to another server"},
		{api, 404, "/models", "No Grok API was found at api.x.ai (HTTP 404)."},
		{api, 404, "/chat/completions", "Check that model grok-4.3 is available to your account"},
		{api, 503, "/chat/completions", "Grok is having trouble right now (HTTP 503)."},
		{api, 418, "/chat/completions", "Grok sent an unexpected response (HTTP 418)."},
	} {
		if err := c.g.statusError(c.code, c.endpoint); !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d %s: %v", c.code, c.endpoint, err)
		}
	}
	var rejected authError
	if !errors.As(api.statusError(401, "/models"), &rejected) || rejected.status != 401 {
		t.Fatal("setup can't recognize a rejected key")
	}
}

func TestConnectionErrorNamesTheServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	base := server.URL
	server.Close()
	c := testConfig(t, base)
	g, _ := NewGrok(context.Background(), c)
	defer g.Close()
	if err := g.Warm(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "Couldn't connect to Grok at "+strings.TrimPrefix(base, "http://")) {
		t.Fatal(err)
	}
}

func TestTimeoutSuggestsLongerTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := testConfig(t, server.URL)
	c.RequestTimeout = 100 * time.Millisecond
	g, _ := NewGrok(context.Background(), c)
	defer g.Close()
	_, err := g.Generate(context.Background(), Request{Model: c.Model})
	if err == nil || err.Error() != "Grok didn't answer within 100ms. Try again, or allow more time with --timeout 200ms" {
		t.Fatal(err)
	}
}
