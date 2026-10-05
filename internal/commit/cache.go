package commit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type cacheEntry struct {
	Subject string    `json:"subject"`
	Created time.Time `json:"created"`
}

func cachePath(c Config, r Request) string {
	b, _ := json.Marshal([]string{"1", c.BaseURL, r.Model, r.Reasoning, r.System, r.Prompt})
	return filepath.Join(c.StateDir, "subjects", digest(b)+".json")
}

func cached(c Config, r Request) (Result, bool) {
	if r.NoCache {
		return Result{}, false
	}
	b, err := os.ReadFile(cachePath(c, r))
	if err != nil {
		return Result{}, false
	}
	var entry cacheEntry
	if json.Unmarshal(b, &entry) != nil || time.Since(entry.Created) < 0 || time.Since(entry.Created) > 7*24*time.Hour {
		return Result{}, false
	}
	s, err := ValidateSubject(entry.Subject)
	if err != nil {
		return Result{}, false
	}
	return Result{Subject: s, Cached: true}, true
}

func generateCached(ctx context.Context, g *Grok, r Request) (Result, error) {
	if result, ok := cached(g.config, r); ok {
		return result, nil
	}
	s, err := g.Generate(ctx, r)
	if err != nil {
		return Result{}, err
	}
	if !r.NoCache {
		b, _ := json.Marshal(cacheEntry{s, time.Now()})
		// A full/unwritable cache must not invalidate a successfully generated subject.
		if atomicWrite(cachePath(g.config, r), b) == nil {
			pruneCache(g.config)
		}
	}
	return Result{Subject: s}, nil
}

func pruneCache(c Config) {
	entries, _ := os.ReadDir(filepath.Join(c.StateDir, "subjects"))
	type item struct {
		path     string
		modified time.Time
	}
	var files []item
	for _, e := range entries {
		if len(e.Name()) != 69 || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, item{filepath.Join(c.StateDir, "subjects", e.Name()), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.After(files[j].modified) })
	for i, f := range files {
		if i >= 512 || time.Since(f.modified) > 7*24*time.Hour {
			_ = os.Remove(f.path)
		}
	}
}
