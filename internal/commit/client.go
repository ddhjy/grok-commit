package commit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
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
		return nil, fmt.Errorf("Grok connection failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, errors.New("Grok authentication failed; refresh your CLI login or API key")
		}
		if resp.StatusCode == 426 {
			return nil, errors.New("Grok CLI needs updating; update it and restart the grok-commit daemon")
		}
		return nil, fmt.Errorf("Grok request failed (HTTP %d)", resp.StatusCode)
	}
	return resp, nil
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
			return "", errors.New("invalid Grok streaming response")
		}
		if hasJSON(event.Error) {
			return "", errors.New("Grok returned a streaming API error")
		}
		if len(event.Choices) > 1 {
			return "", errors.New("Grok returned multiple choices")
		}
		for _, choice := range event.Choices {
			if finished {
				return "", errors.New("Grok returned text after completion")
			}
			if hasJSON(choice.Delta.Tools) || hasJSON(choice.Delta.Function) || hasJSON(choice.Delta.Refusal) {
				return "", errors.New("Grok returned a tool call or refusal")
			}
			if choice.Delta.Content != nil {
				text.WriteString(*choice.Delta.Content)
			}
			if text.Len() > 4096 {
				return "", errors.New("Grok subject is too long")
			}
			if choice.Finish != nil {
				if *choice.Finish != "stop" {
					return "", errors.New("Grok generation did not finish successfully")
				}
				finished = true
			}
		}
	}
	if scanner.Err() != nil {
		return "", fmt.Errorf("Grok stream interrupted: %w", scanner.Err())
	}
	if !finished {
		return "", errors.New("Grok stream ended without a completed subject")
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

func (g *Grok) Generate(ctx context.Context, r Request) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.config.RequestTimeout)
	defer cancel()
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
