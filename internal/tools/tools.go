// Package tools resolves the external media helpers that the node shells out
// to (FFmpeg and yt-dlp) and installs a missing one on demand.
//
// A bare helper name only ever means a file inside the configured tools
// directory; PATH is never consulted, so a node always runs the build it was
// given rather than whatever the machine happens to have installed. A missing
// helper is downloaded for the current platform into that directory, where it is
// reused on later starts.
package tools

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// MinSize is the smallest download that is treated as a real executable. A
// shorter body is an error page or a truncated transfer, never a helper.
const MinSize = 1 << 20

// progressInterval is how often a running download reports its position.
const progressInterval = 10 * time.Second

// Resolver locates the media helpers.
type Resolver struct {
	// Directory caches downloaded helpers.
	Directory string
	// Download installs a helper that is not already in Directory.
	Download bool
	// Logger receives download progress. Nil falls back to the default logger.
	Logger *slog.Logger
	// Client performs the downloads. Nil builds a client suited to large files.
	Client *http.Client

	once     sync.Once
	mu       sync.Mutex
	archives map[string]string
	temp     string
}

// log returns the configured logger so callers never have to check for nil.
func (r *Resolver) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Close removes the archives fetched during this run.
func (r *Resolver) Close() error {
	r.mu.Lock()
	temp := r.temp
	r.temp = ""
	r.mu.Unlock()
	if temp == "" {
		return nil
	}
	return os.RemoveAll(temp)
}

// Resolve returns a runnable path for name. configured is the value from the
// configuration file: an explicit path, or a bare helper name that is looked for
// in the tools directory. A bare name is never looked up on PATH, so every node
// runs the same build of a helper whatever the machine happens to have installed.
func (r *Resolver) Resolve(ctx context.Context, name, configured string) (string, error) {
	if configured == "" {
		configured = name
	}
	// An explicit path is a deliberate choice, so it is never replaced by a
	// download: a broken path should be reported, not papered over.
	if strings.ContainsAny(configured, `/\`) {
		found, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("%s is configured as %q but cannot be run: %w", name, configured, err)
		}
		return found, nil
	}
	cached := filepath.Join(r.Directory, executableName(name))
	if info, err := os.Stat(cached); err == nil && !info.IsDir() {
		if absolute, err := filepath.Abs(cached); err == nil {
			return absolute, nil
		}
		return cached, nil
	}
	if !r.Download {
		return "", fmt.Errorf("%s was not found in %s and autoDownload is off; put a copy there or turn autoDownload on", name, r.Directory)
	}
	return r.install(ctx, name)
}

// install downloads and unpacks the helper for the current platform.
func (r *Resolver) install(ctx context.Context, name string) (string, error) {
	src, err := sourceFor(name)
	if err != nil {
		return "", err
	}
	if r.Directory == "" {
		return "", fmt.Errorf("toolsDirectory is empty, so %s has nowhere to be installed", name)
	}
	if err := os.MkdirAll(r.Directory, 0o755); err != nil {
		return "", fmt.Errorf("tools directory %s: %w", r.Directory, err)
	}
	log := r.log()
	start := time.Now()
	log.Info("downloading media helper", "tool", name, "url", src.urls[0])
	archive, err := r.fetch(ctx, src)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", name, err)
	}
	target := filepath.Join(r.Directory, executableName(name))
	if src.member == "" {
		err = installFile(archive, target)
	} else {
		err = extract(archive, src.member, target)
	}
	if err != nil {
		return "", fmt.Errorf("installing %s: %w", name, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, 0o755); err != nil {
			return "", fmt.Errorf("installing %s: %w", name, err)
		}
	}
	log.Info("media helper installed", "tool", name, "path", target, "duration", time.Since(start).Round(time.Millisecond))
	if absolute, err := filepath.Abs(target); err == nil {
		return absolute, nil
	}
	return target, nil
}

// fetch downloads the first reachable URL of a source and returns the local
// archive. Downloads are memoised, so one archive shared by several helpers is
// only transferred once.
func (r *Resolver) fetch(ctx context.Context, src source) (string, error) {
	key := strings.Join(src.urls, " ")
	r.mu.Lock()
	if file, ok := r.archives[key]; ok {
		r.mu.Unlock()
		return file, nil
	}
	dir := r.temp
	if dir == "" {
		created, err := os.MkdirTemp("", "golalink-tools-")
		if err != nil {
			r.mu.Unlock()
			return "", err
		}
		dir, r.temp = created, created
	}
	r.mu.Unlock()

	var lastErr error
	for _, url := range src.urls {
		file, err := r.download(ctx, url, dir, src)
		if err == nil {
			r.remember(key, file)
			return file, nil
		}
		lastErr = err
		r.log().Debug("media helper download failed", "url", url, "error", err)
	}
	return "", lastErr
}

// remember records a downloaded archive so the next helper served from the same
// archive does not transfer it again.
func (r *Resolver) remember(key, file string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.archives == nil {
		r.archives = map[string]string{}
	}
	r.archives[key] = file
}

// download streams one URL into dir and verifies it.
func (r *Resolver) download(ctx context.Context, url, dir string, src source) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "golalink")
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	name := src.asset
	if name == "" {
		name = path.Base(resp.Request.URL.Path)
	}
	if name == "" || name == "." || name == "/" {
		name = "download.bin"
	}
	file := filepath.Join(dir, name)
	out, err := os.Create(file)
	if err != nil {
		return "", err
	}
	var body io.Reader = resp.Body
	if resp.ContentLength > 0 {
		body = &progress{reader: resp.Body, tool: src.tool, total: resp.ContentLength, next: time.Now().Add(progressInterval), log: r.log()}
	}
	written, err := io.Copy(out, body)
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file)
		return "", err
	}
	if written < MinSize {
		os.Remove(file)
		return "", fmt.Errorf("%s returned only %d bytes", url, written)
	}
	if src.sums != "" {
		if err := verify(ctx, r.httpClient(), src.sums, name, file); err != nil {
			os.Remove(file)
			return "", err
		}
	}
	return file, nil
}

func (r *Resolver) httpClient() *http.Client {
	r.once.Do(func() {
		if r.Client != nil {
			return
		}
		r.Client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
		}}
	})
	return r.Client
}

// progress reports the position of a long download so a slow link does not look
// like a hang.
type progress struct {
	reader  io.Reader
	tool    string
	total   int64
	written int64
	next    time.Time
	log     *slog.Logger
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.written += int64(n)
	if time.Now().After(p.next) {
		p.next = time.Now().Add(progressInterval)
		p.log.Info("media helper download in progress",
			"tool", p.tool,
			"receivedMiB", p.written>>20,
			"totalMiB", p.total>>20,
		)
	}
	return n, err
}

// executableName is the file name a helper is installed under.
func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// installFile places a downloaded executable at target. The bytes are copied
// rather than renamed, because the download directory and the tools directory
// are not always on the same filesystem.
func installFile(from, target string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	return copyInto(src, target)
}

// extract copies one member of a zip archive onto target.
func extract(archive, member, target string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() || path.Base(entry.Name) != member {
			continue
		}
		src, err := entry.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		return copyInto(src, target)
	}
	return fmt.Errorf("%s is not part of %s", member, filepath.Base(archive))
}

// copyInto writes src to target through a temporary file, so a target that is
// still in use is never left half written.
func copyInto(src io.Reader, target string) error {
	temp := target + ".part"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(temp)
		return err
	}
	if err = out.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	return os.Rename(temp, target)
}

// verify checks a download against a "<sha256>  <file>" listing.
func verify(ctx context.Context, client *http.Client, sumsURL, asset, file string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "golalink")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("checksum list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum list %s returned HTTP %d", sumsURL, resp.StatusCode)
	}
	list, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("checksum list: %w", err)
	}
	want := lookupSum(string(list), asset)
	if want == "" {
		return fmt.Errorf("checksum list %s has no entry for %s", sumsURL, asset)
	}
	got, err := fileSum(file)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("%s checksum mismatch: expected %s, downloaded %s", asset, want, got)
	}
	return nil
}

// lookupSum finds the hash for asset in a checksum listing.
func lookupSum(list, asset string) string {
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0]
		}
	}
	return ""
}

func fileSum(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
