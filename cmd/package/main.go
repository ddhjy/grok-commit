// Command package creates release archives and SHA-256 checksums using only Go.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	version := flag.String("version", "0.2.0", "release version without v")
	flag.Parse()
	if *version == "" || strings.ContainsAny(*version, " /\\\n\r\t") {
		return fmt.Errorf("invalid version")
	}
	if err := os.MkdirAll("dist", 0755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "grok-commit-package-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var checksums strings.Builder
	for _, target := range []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"} {
		parts := strings.Split(target, "/")
		name := "grok-commit"
		if parts[0] == "windows" {
			name += ".exe"
		}
		binary := filepath.Join(work, name)
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -X main.version="+*version, "-o", binary, "./cmd/grok-commit")
		cmd.Env = append(os.Environ(), "GOOS="+parts[0], "GOARCH="+parts[1], "CGO_ENABLED=0")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		files := map[string]string{name: binary, "README.md": "README.md", "LICENSE": "LICENSE"}
		archive := fmt.Sprintf("grok-commit_%s_%s_%s", *version, parts[0], parts[1])
		if parts[0] == "windows" {
			archive += ".zip"
		} else {
			archive += ".tar.gz"
		}
		path := filepath.Join("dist", archive)
		if err := pack(path, files); err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&checksums, "%x  %s\n", sha256.Sum256(b), archive)
		fmt.Println(archive)
	}
	return os.WriteFile("dist/checksums.txt", []byte(checksums.String()), 0644)
}

func pack(path string, files map[string]string) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if strings.HasSuffix(path, ".zip") {
		w := zip.NewWriter(f)
		for _, name := range names {
			source := files[name]
			b, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(0644)
			if strings.HasSuffix(name, ".exe") {
				header.SetMode(0755)
			}
			entry, err := w.CreateHeader(header)
			if err != nil {
				return err
			}
			if _, err = entry.Write(b); err != nil {
				return err
			}
		}
		return w.Close()
	}
	gz := gzip.NewWriter(f)
	w := tar.NewWriter(gz)
	for _, name := range names {
		source := files[name]
		b, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		mode := int64(0644)
		if name == "grok-commit" {
			mode = 0755
		}
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b))}); err != nil {
			return err
		}
		if _, err := io.Copy(w, strings.NewReader(string(b))); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return gz.Close()
}
