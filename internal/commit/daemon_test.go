package commit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestDaemonLifecycleAndCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix connection service")
	}
	var generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		generations.Add(1)
		sse(w, "feat: daemon generated subject", "stop")
	}))
	defer server.Close()
	c := testConfig(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, c, false) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	if err := waitDaemon(ctx, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socketPath(c))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("socket permissions")
	}
	var result Result
	if err := localCall(ctx, c, "/warm", nil, &result); err != nil || result.Error != "" {
		t.Fatalf("warm: %v %v", err, result)
	}
	r := Request{Model: c.Model, Reasoning: c.Reasoning, System: systemPrompt, Prompt: "+some change"}
	for i := 0; i < 2; i++ {
		result = Result{}
		if err := localCall(ctx, c, "/generate", r, &result); err != nil {
			t.Fatal(err)
		}
		if result.Subject != "feat: daemon generated subject" || result.Error != "" {
			t.Fatalf("%+v", result)
		}
		if result.Cached != (i == 1) {
			t.Fatalf("unexpected cache status: %v", result)
		}
	}
	if generations.Load() != 1 {
		t.Fatalf("generations=%d", generations.Load())
	}
	if err := stopDaemon(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerLockPreventsDuplicateServices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix connection service")
	}
	path := t.TempDir() + "/worker.lock"
	unlocked, err := lockWorker(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockWorker(path); err == nil {
		second()
		unlocked()
		t.Fatal("duplicate worker accepted")
	}
	unlocked()
	third, err := lockWorker(path)
	if err != nil {
		t.Fatal(err)
	}
	third()
}
