package commit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

func socketPath(c Config) string {
	// File-backed credentials are re-read for each request, so login refreshes do
	// not change the service address. Distinguish shell-only API keys, which a
	// running child cannot inherit after they change.
	key := ""
	if c.Auth == "api" {
		key = os.Getenv("XAI_API_KEY")
		var saved struct {
			APIKey string `json:"api_key"`
		}
		if b, err := os.ReadFile(filepath.Join(c.ConfigDir, "credentials.json")); err == nil && json.Unmarshal(b, &saved) == nil && strings.TrimSpace(key) == saved.APIKey {
			key = ""
		}
	}
	identity := strings.Join([]string{"1", c.Version, c.StateDir, c.ConfigDir, c.Auth, os.Getenv("GROK_HOME"), c.BaseURL, c.Timeout, c.HedgeDelay, digest([]byte(key))}, "\x00")
	return filepath.Join(os.TempDir(), "grok-commit-"+digest([]byte(identity))[:16], "worker.sock")
}

func localClient(c Config) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socketPath(c))
	}}, Timeout: c.RequestTimeout + 2*time.Second}
}

func localCall(ctx context.Context, c Config, endpoint string, input any, out any) error {
	var data []byte
	if input != nil {
		var err error
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost"+endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	client := localClient(c)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Couldn't reach the background service. Run grok-commit daemon stop and try again; to commit without it, add --no-daemon.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("The background service returned an error (HTTP %d). Run grok-commit daemon stop and try again; to commit without it, add --no-daemon.", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(out)
}

func daemonReady(ctx context.Context, c Config) bool {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	var result Result
	return localCall(ctx, c, "/health", nil, &result) == nil
}

func startDaemon(ctx context.Context, c Config) error {
	if runtime.GOOS == "windows" {
		return errors.New("The background service is available only on macOS and Linux.")
	}
	if daemonReady(ctx, c) {
		return nil
	}
	if err := privateDir(c.StateDir); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(c.StateDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Env = append(os.Environ(), "GROK_COMMIT_AUTH="+c.Auth, "GROK_COMMIT_BASE_URL="+c.BaseURL,
		"GROK_COMMIT_TIMEOUT="+c.Timeout, "GROK_COMMIT_HEDGE_DELAY="+c.HedgeDelay,
		"GROK_COMMIT_STATE_DIR="+c.StateDir, "GROK_COMMIT_CONFIG_DIR="+c.ConfigDir)
	cmd.Stdout = log
	cmd.Stderr = log
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return waitDaemon(ctx, c)
}

func waitDaemon(ctx context.Context, c Config) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			if daemonReady(ctx, c) {
				return nil
			}
		case <-timeout.C:
			return fmt.Errorf("The background service didn't start within 3 seconds. Details are in %s. To commit without it, add --no-daemon.", filepath.Join(c.StateDir, "daemon.log"))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func stopDaemon(ctx context.Context, c Config) error {
	if !daemonReady(ctx, c) {
		return nil
	}
	var r Result
	if err := localCall(ctx, c, "/stop", nil, &r); err != nil {
		return err
	}
	for i := 0; i < 200; i++ {
		if !daemonReady(ctx, c) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return errors.New("The background service is still shutting down. Wait a moment, then try again.")
}

func runDaemon(ctx context.Context, c Config, keepalive bool) error {
	path := socketPath(c)
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	unlock, err := lockWorker(filepath.Join(filepath.Dir(path), "worker.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	g, err := NewGrok(ctx, c)
	if err != nil {
		return err
	}
	defer g.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var last atomic.Int64
	last.Store(time.Now().Unix())
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		last.Store(time.Now().Unix())
		w.Header().Set("Content-Type", "application/json")
		result := Result{}
		switch r.URL.Path {
		case "/health":
		case "/stop":
			defer cancel()
		case "/warm":
			if e := g.Warm(r.Context()); e != nil {
				result.Error = e.Error()
			}
		case "/generate":
			var request Request
			err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&request)
			if err != nil || !strings.HasPrefix(request.Model, "grok-") || request.System == "" || request.Prompt == "" {
				result.Error = "The background service received an incomplete request. Run grok-commit daemon stop, then try again."
			} else {
				result, err = generateCached(r.Context(), g, request)
				if err != nil {
					result.Error = err.Error()
				}
			}
		default:
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	replaced := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		grace := time.Second
		select {
		case <-ctx.Done():
		case <-replaced:
			// Let in-flight commits finish before retiring the old version.
			grace = c.RequestTimeout + 5*time.Second
		}
		shutdown, stop := context.WithTimeout(context.Background(), grace)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	exe, _ := executablePath()
	original, _ := os.Stat(exe)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if current, e := os.Stat(exe); e == nil && original != nil && !os.SameFile(original, current) {
					close(replaced)
					return
				}
				if !keepalive && time.Since(time.Unix(last.Load(), 0)) > 20*time.Minute {
					cancel()
					return
				}
				_ = g.Warm(ctx)
			}
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped
		return nil
	}
	return err
}

func generate(ctx context.Context, c Config, r Request, noDaemon bool) (Result, error) {
	if result, ok := cached(c, r); ok {
		return result, nil
	}
	if !noDaemon && runtime.GOOS != "windows" {
		if err := startDaemon(ctx, c); err != nil {
			return Result{}, err
		}
		var result Result
		if err := localCall(ctx, c, "/generate", r, &result); err != nil {
			return Result{}, err
		}
		if result.Error != "" {
			return Result{}, errors.New(result.Error)
		}
		var err error
		result.Subject, err = ValidateSubject(result.Subject)
		return result, err
	}
	g, err := NewGrok(ctx, c)
	if err != nil {
		return Result{}, err
	}
	defer g.Close()
	return generateCached(ctx, g, r)
}
