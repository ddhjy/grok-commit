package commit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const releaseRepository = "ddhjy/grok-commit"
const latestReleaseURL = "https://api.github.com/repos/" + releaseRepository + "/releases/latest"

var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var (
	errBadPackage        = errors.New("The release package doesn't contain a valid grok-commit program, so nothing was installed.")
	errUntrustedRedirect = errors.New("GitHub sent the download to an unexpected server, so nothing was downloaded.")
)

func versionParts(s string) ([3]uint64, error) {
	var parts [3]uint64
	m := stableVersion.FindStringSubmatch(s)
	if len(m) != 4 {
		return parts, errors.New("expected a stable release version")
	}
	for i := range parts {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return parts, err
		}
		parts[i] = n
	}
	return parts, nil
}

func newer(a, b string) bool {
	aa, err := versionParts(a)
	if err != nil {
		return false
	}
	bb, err := versionParts(b)
	if err != nil {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return aa[i] > bb[i]
		}
	}
	return false
}

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type distribution struct {
	client        *http.Client
	latestURL     string
	goos, arch    string
	validateAsset func(string, string, string) bool
	verifyBinary  func(context.Context, string, string) error
}

func newDistribution() distribution {
	return distribution{client: &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 5 || r.URL.Scheme != "https" {
			return errUntrustedRedirect
		}
		switch r.URL.Host {
		case "github.com", "api.github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
			return nil
		}
		return errUntrustedRedirect
	}}, latestURL: latestReleaseURL, goos: runtime.GOOS, arch: runtime.GOARCH, validateAsset: trustedAsset, verifyBinary: verifyVersion}
}

func trustedAsset(raw, tag, name string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "/"+releaseRepository+"/releases/download/"+tag+"/"+name
}

func (d distribution) get(ctx context.Context, raw string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "grok-commit-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		if errors.Is(err, errUntrustedRedirect) {
			return nil, errUntrustedRedirect
		}
		return nil, errors.New("Couldn't reach GitHub. Check your internet connection, then try again.")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		if resp.StatusCode == 403 || resp.StatusCode == 429 {
			return nil, fmt.Errorf("GitHub is limiting requests right now (HTTP %d). Try again in an hour.", resp.StatusCode)
		}
		return nil, fmt.Errorf("GitHub returned an error (HTTP %d). Try again later.", resp.StatusCode)
	}
	return resp, nil
}

func (d distribution) latest(ctx context.Context) (release, error) {
	var r release
	resp, err := d.get(ctx, d.latestURL)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return r, errors.New("Couldn't read the release information from GitHub. Try again later.")
	}
	if _, err = versionParts(r.Tag); err != nil || r.Draft || r.Prerelease {
		return r, errors.New("The latest release on GitHub isn't a stable version, so it was skipped.")
	}
	return r, nil
}

func (d distribution) download(ctx context.Context, r release, dir string) (string, error) {
	ext := ".tar.gz"
	binary := "grok-commit"
	if d.goos == "windows" {
		ext = ".zip"
		binary += ".exe"
	}
	name := fmt.Sprintf("grok-commit_%s_%s_%s%s", strings.TrimPrefix(r.Tag, "v"), d.goos, d.arch, ext)
	assets := map[string]string{}
	for _, a := range r.Assets {
		if a.Name == name || a.Name == "checksums.txt" {
			if assets[a.Name] != "" || !d.validateAsset(a.URL, r.Tag, a.Name) {
				return "", errors.New("The release's download links don't point to the official repository, so nothing was installed.")
			}
			assets[a.Name] = a.URL
		}
	}
	if len(assets) != 2 {
		return "", fmt.Errorf("Release %s doesn't include a verified package for %s/%s, so nothing was installed.", strings.TrimPrefix(r.Tag, "v"), d.goos, d.arch)
	}
	resp, err := d.get(ctx, assets["checksums.txt"])
	if err != nil {
		return "", err
	}
	manifest, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	resp.Body.Close()
	if err != nil || len(manifest) > 65536 {
		return "", errors.New("The release's checksum file is damaged, so nothing was installed.")
	}
	expected, err := manifestHash(string(manifest), name)
	if err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(dir, ".grok-commit-archive-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	resp, err = d.get(ctx, assets[name])
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, (64<<20)+1))
	resp.Body.Close()
	if err != nil || n > 64<<20 {
		return "", errors.New("The download didn't finish, or was larger than expected, so nothing was installed. Try again.")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return "", errors.New("The download didn't match its published checksum, so it wasn't installed. Your current version is unchanged.")
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	candidate, err := os.CreateTemp(dir, ".grok-commit-next-*")
	if err != nil {
		return "", err
	}
	path := candidate.Name()
	success := false
	defer func() {
		candidate.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if ext == ".zip" {
		err = extractZIP(archive, n, binary, candidate)
	} else {
		err = extractTar(archive, binary, candidate)
	}
	if err != nil {
		return "", err
	}
	if err = candidate.Close(); err != nil {
		return "", err
	}
	if err = os.Chmod(path, 0755); err != nil {
		return "", err
	}
	if d.goos == "windows" {
		next := path + ".exe"
		if err = os.Rename(path, next); err != nil {
			return "", err
		}
		path = next
	}
	if err = d.verifyBinary(ctx, path, strings.TrimPrefix(r.Tag, "v")); err != nil {
		return "", err
	}
	success = true
	return path, nil
}

func manifestHash(manifest, name string) (string, error) {
	var found string
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		b, err := hex.DecodeString(fields[0])
		if err != nil || len(b) != 32 || found != "" {
			return "", errors.New("The release's checksum file is damaged, so nothing was installed.")
		}
		found = strings.ToLower(fields[0])
	}
	if found == "" {
		return "", errors.New("The release's checksum file doesn't list this package, so nothing was installed.")
	}
	return found, nil
}

func extractTar(archive io.Reader, name string, dst io.Writer) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, 128<<20))
	found := false
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Name != name {
			continue
		}
		if found || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > 64<<20 {
			return errBadPackage
		}
		found = true
		if _, err = io.Copy(dst, reader); err != nil {
			return err
		}
	}
	if !found {
		return errBadPackage
	}
	return nil
}

func extractZIP(archive io.ReaderAt, size int64, name string, dst io.Writer) error {
	r, err := zip.NewReader(archive, size)
	if err != nil {
		return err
	}
	found := false
	for _, f := range r.File {
		if f.Name != name {
			continue
		}
		if found || !f.Mode().IsRegular() || f.UncompressedSize64 == 0 || f.UncompressedSize64 > 64<<20 {
			return errBadPackage
		}
		found = true
		in, err := f.Open()
		if err != nil {
			return err
		}
		n, err := io.Copy(dst, io.LimitReader(in, (64<<20)+1))
		in.Close()
		if err != nil {
			return err
		}
		if n > 64<<20 {
			return errBadPackage
		}
	}
	if !found {
		return errBadPackage
	}
	return nil
}

func verifyVersion(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil || strings.TrimSpace(string(b)) != "grok-commit "+version {
		return errors.New("The downloaded program didn't pass its version check, so it wasn't installed.")
	}
	return nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dst)
		return copyErr
	}
	return closeErr
}

func replaceExecutable(ctx context.Context, candidate, target string) error {
	// Rename within one filesystem is atomic on Unix. On Windows another process
	// may still hold the old image; the detached helper waits without interrupting it.
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := os.Rename(candidate, target)
		if err == nil {
			return nil
		}
		if runtime.GOOS != "windows" || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func previousPath(target string) string {
	if runtime.GOOS == "windows" {
		return target + ".previous.exe"
	}
	return target + ".previous"
}

func installCandidate(ctx context.Context, candidate, target string) (string, error) {
	previous := previousPath(target)
	file, err := os.CreateTemp(filepath.Dir(target), ".grok-commit-backup-*")
	if err != nil {
		return "", err
	}
	tmp := file.Name()
	file.Close()
	os.Remove(tmp)
	defer os.Remove(tmp)
	if err = copyExecutable(target, tmp); err != nil {
		return "", err
	}
	hash, err := fileHash(tmp)
	if err != nil {
		return "", err
	}
	if err = replaceExecutable(ctx, candidate, target); err != nil {
		return "", err
	}
	if err = os.Rename(tmp, previous); err != nil {
		// Publishing the backup is part of the transaction. Keep the earlier
		// backup and restore the current executable if it cannot be published.
		recovery, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if restoreErr := replaceExecutable(recovery, tmp, target); restoreErr != nil {
			return "", fmt.Errorf("the backup failed (%v) and so did restoring the previous program (%w); reinstall grok-commit with the installer", err, restoreErr)
		}
		return "", err
	}
	return hash, nil
}
