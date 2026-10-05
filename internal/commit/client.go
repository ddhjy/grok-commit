package commit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type Request struct {
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
	System    string `json:"system"`
	Prompt    string `json:"prompt"`
	NoCache   bool   `json:"no_cache"`
}

type Result struct {
	Subject string `json:"subject,omitempty"`
	Cached  bool   `json:"cached,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Grok struct {
	config  Config
	client  *http.Client
	version string
}

func NewGrok(ctx context.Context, c Config) (*Grok, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 4
	t.IdleConnTimeout = 90 * time.Second
	t.ResponseHeaderTimeout = c.RequestTimeout
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	g := &Grok{config: c, client: &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if c.Auth == "cli" {
		var err error
		g.version, err = cliVersion(ctx)
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}

func (g *Grok) Close() { g.client.CloseIdleConnections() }

func (g *Grok) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	key, err := g.config.credential()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, g.config.BaseURL+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grok-commit/1")
	if g.version != "" {
		req.Header.Set("x-grok-client-version", g.version)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, g.connectionError(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, g.statusError(resp.StatusCode, endpoint)
	}
	return resp, nil
}

// authError means Grok rejected the credentials themselves, so setup can ask
// for a different key instead of giving up.
type authError struct {
	status  int
	message string
}

func (e authError) Error() string { return e.message }

func (g *Grok) statusError(code int, endpoint string) error {
	switch {
	case g.config.Auth == "cli" && (code == 401 || code == 403):
		return authError{code, "Grok didn't accept your Grok CLI sign-in; it may have expired. Run grok login, then try again."}
	case code == 401:
		return authError{code, "Grok didn't accept your API key (HTTP 401). Check that it's complete and still active at https://console.x.ai, then save it again with: grok-commit setup --auth api"}
	case code == 403:
		return authError{code, "Grok refused this API key (HTTP 403). The key may not have access to this model, or your account may be out of credits. Check https://console.x.ai"}
	case code == 426 && runtime.GOOS == "windows":
		return errors.New("Grok needs a newer Grok CLI. Update Grok CLI, then try again.")
	case code == 426:
		return errors.New("Grok needs a newer Grok CLI. Update Grok CLI, then run grok-commit daemon stop so grok-commit picks up the new version.")
	case code == 429:
		return errors.New("Grok is limiting requests right now (HTTP 429). Wait a minute, then try again.")
	case code >= 300 && code < 400:
		return fmt.Errorf("The API address redirected elsewhere (HTTP %d). grok-commit doesn't follow redirects, so your key is never sent to another server. Check the API address (--base-url).", code)
	case code == 404 && endpoint == "/models":
		return fmt.Errorf("No Grok API was found at %s (HTTP 404). Check the API address (--base-url).", g.host())
	case code == 400 || code == 404 || code == 422:
		return fmt.Errorf("Grok rejected the request (HTTP %d). Check that model %s is available to your account and supports reasoning level %s.", code, g.config.Model, g.config.Reasoning)
	case code >= 500:
		return fmt.Errorf("Grok is having trouble right now (HTTP %d). Try again in a moment.", code)
	}
	return fmt.Errorf("Grok sent an unexpected response (HTTP %d). Try again in a moment.", code)
}

func (g *Grok) connectionError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	host := g.host()
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return fmt.Errorf("Grok at %s took too long to respond. Check your internet connection, then try again.", host)
	case errors.As(err, &dns):
		return fmt.Errorf("Couldn't find %s. Check your internet connection and the API address, then try again.", host)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("Couldn't connect to Grok at %s (connection refused). Check the API address, or try again later.", host)
	case errors.As(err, &cert):
		return fmt.Errorf("Couldn't verify the security certificate of %s, so nothing was sent. If your network inspects HTTPS traffic, ask your network administrator; otherwise check the API address.", host)
	}
	return fmt.Errorf("Couldn't connect to Grok at %s. Check your internet connection, then try again.", host)
}

func (g *Grok) host() string {
	if u, err := url.Parse(g.config.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return g.config.BaseURL
}

func (g *Grok) Warm(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := g.request(ctx, "GET", "/models", nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(r.Body, 2<<20))
	return err
}

func (g *Grok) generateOnce(ctx context.Context, r Request) (string, error) {
	payload := map[string]any{"model": r.Model, "reasoning_effort": r.Reasoning,
		"messages": []map[string]string{{"role": "system", "content": r.System}, {"role": "user", "content": r.Prompt}},
		"stream":   true, "store": false, "max_tokens": 512, "stream_options": map[string]bool{"include_usage": true}}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	resp, err := g.request(ctx, "POST", "/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return parseStream(resp.Body)
}

func parseStream(body io.Reader) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var text strings.Builder
	finished := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content  *string         `json:"content"`
					Tools    json.RawMessage `json:"tool_calls"`
					Function json.RawMessage `json:"function_call"`
					Refusal  json.RawMessage `json:"refusal"`
				} `json:"delta"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return "", errors.New("Grok sent a response grok-commit couldn't read. Try again.")
		}
		if hasJSON(event.Error) {
			return "", errors.New("Grok reported an error partway through. Try again.")
		}
		if len(event.Choices) > 1 {
			return "", errors.New("Grok sent an unexpected response. Try again.")
		}
		for _, choice := range event.Choices {
			if finished || hasJSON(choice.Delta.Tools) || hasJSON(choice.Delta.Function) {
				return "", errors.New("Grok sent an unexpected response. Try again.")
			}
			if hasJSON(choice.Delta.Refusal) {
				return "", errors.New("Grok declined to write a subject for these changes. Try again, or write this one yourself with git commit.")
			}
			if choice.Delta.Content != nil {
				text.WriteString(*choice.Delta.Content)
			}
			if text.Len() > 4096 {
				return "", errors.New("Grok's answer was far too long for a commit subject. Try again.")
			}
			if choice.Finish != nil {
				if *choice.Finish != "stop" {
					return "", errors.New("Grok stopped before finishing the subject. Try again.")
				}
				finished = true
			}
		}
	}
	if scanner.Err() != nil {
		return "", errors.New("The connection to Grok dropped before the subject was finished. Try again.")
	}
	if !finished {
		return "", errors.New("Grok's answer ended early, before the subject was finished. Try again.")
	}
	// Drain the short HTTP trailer so the transport can reuse the connection.
	if _, err := io.Copy(io.Discard, io.LimitReader(body, 65536)); err != nil {
		return "", err
	}
	return ValidateSubject(text.String())
}

func hasJSON(b json.RawMessage) bool {
	s := string(b)
	return len(b) > 0 && s != "null" && s != `""` && s != "[]"
}

func (g *Grok) Generate(ctx context.Context, r Request) (subject string, err error) {
	ctx, cancel := context.WithTimeout(ctx, g.config.RequestTimeout)
	defer cancel()
	defer func() {
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t := g.config.RequestTimeout
			more := "Try again in a moment."
			if t < 5*time.Minute {
				more = "Try again, or allow more time with --timeout " + duration(min(2*t, 5*time.Minute))
			}
			err = fmt.Errorf("Grok didn't answer within %s. %s", duration(t), more)
		}
	}()
	type outcome struct {
		subject string
		err     error
	}
	results := make(chan outcome, 2)
	start := func() { go func() { s, err := g.generateOnce(ctx, r); results <- outcome{s, err} }() }
	start()
	if g.config.Hedge == 0 {
		select {
		case o := <-results:
			return o.subject, o.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	timer := time.NewTimer(g.config.Hedge)
	defer timer.Stop()
	var firstErr error
	select {
	case o := <-results:
		if o.err == nil {
			return o.subject, nil
		}
		firstErr = o.err
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	start()
	remaining := 2
	if firstErr != nil {
		remaining = 1
	}
	for i := 0; i < remaining; i++ {
		select {
		case o := <-results:
			if o.err == nil {
				return o.subject, nil
			}
			if firstErr == nil {
				firstErr = o.err
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", firstErr
}
