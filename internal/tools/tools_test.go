package tools

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// roundTripFunc lets a case use a client that never reaches the network, so a
// path that should not download fails at that rather than at a slow timeout.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func offlineClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the test tried to reach the network")
	})}
}

func TestLookupSum(t *testing.T) {
	cases := []struct {
		name  string
		list  string
		asset string
		want  string
	}{
		{
			name:  "the entry for the asset is the one taken",
			list:  "aaa111  yt-dlp.exe\nbbb222  yt-dlp_arm64.exe\n",
			asset: "yt-dlp_arm64.exe",
			want:  "bbb222",
		},
		{
			// The published listings arrive with the line endings the service
			// wrote them with.
			name:  "carriage returns do not end up in the hash",
			list:  "aaa111  ffmpeg.zip\r\nbbb222  ffprobe.zip\r\n",
			asset: "ffprobe.zip",
			want:  "bbb222",
		},
		{
			name:  "several spaces between the fields are not a different line",
			list:  "aaa111    ffmpeg.zip",
			asset: "ffmpeg.zip",
			want:  "aaa111",
		},
		{
			// A listing that covers the signature file does not cover the tool,
			// and taking it would refuse a download the service published.
			name:  "a longer name is not a match",
			list:  "aaa111  yt-dlp.exe.sig\nbbb222  yt-dlp.exe\n",
			asset: "yt-dlp.exe",
			want:  "bbb222",
		},
		{
			name:  "an asset the listing does not mention has no sum",
			list:  "aaa111  yt-dlp.exe\n",
			asset: "ffmpeg.zip",
			want:  "",
		},
		{
			// A line with more than the two fields is not the published shape,
			// so it is not read as one.
			name:  "a line with three fields is ignored",
			list:  "aaa111  bbb222  ffmpeg.zip\n",
			asset: "ffmpeg.zip",
			want:  "",
		},
		{name: "an empty listing has no sum", list: "", asset: "yt-dlp.exe", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lookupSum(c.list, c.asset); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

type archiveEntry struct {
	name string
	body string
}

func writeArchive(t *testing.T, entries []archiveEntry) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "helper.zip")
	out, err := os.Create(file)
	if err != nil {
		t.Fatalf("creating the archive: %v", err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("adding %s: %v", e.name, err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatalf("writing %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the archive: %v", err)
	}
	return file
}

// TestExtractTakesTheMemberByBaseName is the decision about which file in an
// archive is the helper: the one whose name matches, wherever the archive puts
// it, because the encoder sits in a directory of its own in the release build.
func TestExtractTakesTheMemberByBaseName(t *testing.T) {
	archive := writeArchive(t, []archiveEntry{
		{"ffmpeg-master-latest-win64-gpl/bin/", ""},
		{"ffmpeg-master-latest-win64-gpl/bin/ffprobe.exe", "probe bytes"},
		{"ffmpeg-master-latest-win64-gpl/bin/ffmpeg.exe", "encoder bytes"},
		{"ffmpeg-master-latest-win64-gpl/bin/ffmpeg", "posix bytes"},
	})

	for _, tc := range []struct{ member, want string }{
		{"ffmpeg.exe", "encoder bytes"},
		{"ffmpeg", "posix bytes"},
	} {
		t.Run("extract "+tc.member, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), tc.member)
			if err := extract(archive, tc.member, target); err != nil {
				t.Fatalf("extract: %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("reading the extracted file: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("extracted %q, want %q", got, tc.want)
			}
			if _, err := os.Stat(target + ".part"); !os.IsNotExist(err) {
				t.Fatalf("a partially written file was left behind: %v", err)
			}
		})
	}

	t.Run("a member the archive does not hold is reported", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "missing")
		err := extract(archive, "nope", target)
		if err == nil {
			t.Fatal("extract succeeded for a member the archive does not hold")
		}
		if !strings.Contains(err.Error(), "is not part of") {
			t.Fatalf("got %v, want it to name the missing member", err)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("a target file was created for a member that was not there: %v", statErr)
		}
	})

	t.Run("a directory entry is not taken as the member", func(t *testing.T) {
		// A release that stored the helper's name as a directory must not be
		// copied as an empty file over the helper.
		dirArchive := writeArchive(t, []archiveEntry{
			{"ffmpeg.exe/", ""},
			{"ffmpeg.exe", "the helper"},
		})
		target := filepath.Join(t.TempDir(), "ffmpeg.exe")
		if err := extract(dirArchive, "ffmpeg.exe", target); err != nil {
			t.Fatalf("extract: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("reading the extracted file: %v", err)
		}
		if want := "the helper"; string(got) != want {
			t.Fatalf("extracted %q, want %q", got, want)
		}
	})
}

// TestInstallFileKeepsTheDownloadedBytes covers the single-file releases, which
// are installed without unpacking.
func TestInstallFileKeepsTheDownloadedBytes(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "download")
	if err := os.WriteFile(from, []byte("single file binary"), 0o755); err != nil {
		t.Fatalf("writing the download: %v", err)
	}
	target := filepath.Join(dir, executableName("yt-dlp"))
	if err := installFile(from, target); err != nil {
		t.Fatalf("installFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading the installed file: %v", err)
	}
	if want := "single file binary"; string(got) != want {
		t.Fatalf("installed %q, want %q", got, want)
	}
	if _, err := os.Stat(target + ".part"); !os.IsNotExist(err) {
		t.Fatalf("a partially written file was left behind: %v", err)
	}
}

// TestResolveBareNameStaysInTheToolsDirectory is the rule the package exists
// for: a bare helper name means a file in the tools directory, never whatever
// the machine happens to have on PATH.
func TestResolveBareNameStaysInTheToolsDirectory(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, executableName("yt-dlp"))
	if err := os.WriteFile(installed, []byte("the build this node was given"), 0o755); err != nil {
		t.Fatalf("writing the cached helper: %v", err)
	}
	// autoDownload is off, so the only possible answer is the copy already
	// there.
	r := &Resolver{Directory: dir, Client: offlineClient()}
	got, err := r.Resolve(context.Background(), "yt-dlp", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != installed {
		t.Fatalf("got %q, want %q", got, installed)
	}
}

func TestResolveMissingBareNameAsksForADownload(t *testing.T) {
	// With autoDownload off, a missing helper is a configuration problem whose
	// message has to say where the file was expected and what to turn on.
	dir := t.TempDir()
	r := &Resolver{Directory: dir, Client: offlineClient()}
	_, err := r.Resolve(context.Background(), "ffmpeg", "")
	if err == nil {
		t.Fatal("Resolve succeeded for a helper that is not in the directory")
	}
	if !strings.Contains(err.Error(), "autoDownload is off") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("got %v, want the directory and the setting to turn on", err)
	}
}

// TestResolveExplicitPathIsTakenAsGiven keeps a deliberate path from being
// papered over with a download: a broken path is reported, not replaced.
func TestResolveExplicitPathIsTakenAsGiven(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	t.Run("a path that cannot be run is reported", func(t *testing.T) {
		r := &Resolver{Directory: dir, Download: true, Client: offlineClient()}
		configured := filepath.Join(dir, "not-installed", executableName("ffmpeg"))
		_, err := r.Resolve(ctx, "ffmpeg", configured)
		if err == nil {
			t.Fatal("Resolve succeeded for a path that does not exist")
		}
		if !strings.Contains(err.Error(), "cannot be run") {
			t.Fatalf("got %v, want it to say the path cannot be run", err)
		}
		// Nothing may have been installed in place of the configured path.
		if _, statErr := os.Stat(filepath.Join(dir, executableName("ffmpeg"))); !os.IsNotExist(statErr) {
			t.Fatalf("a helper was installed in the tools directory instead: %v", statErr)
		}
	})

	t.Run("a path that runs is returned", func(t *testing.T) {
		configured := filepath.Join(dir, executableName("ffmpeg"))
		if err := os.WriteFile(configured, []byte("a build of my own"), 0o755); err != nil {
			t.Fatalf("writing the helper: %v", err)
		}
		r := &Resolver{Directory: dir, Client: offlineClient()}
		got, err := r.Resolve(ctx, "ffmpeg", configured)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got != configured {
			t.Fatalf("got %q, want the configured path %q", got, configured)
		}
	})
}

func TestResolveWithoutADirectoryHasNowhereToInstall(t *testing.T) {
	// A download with no destination is a configuration mistake, and it must be
	// reported before any bytes are moved.
	if _, err := sourceFor("ffmpeg"); err != nil {
		t.Skipf("no FFmpeg source on %s: %v", platform(), err)
	}
	r := &Resolver{Download: true, Client: offlineClient()}
	_, err := r.Resolve(context.Background(), "ffmpeg", "")
	if err == nil {
		t.Fatal("Resolve succeeded with no tools directory")
	}
	if !strings.Contains(err.Error(), "nowhere to be installed") {
		t.Fatalf("got %v, want it to name the empty setting", err)
	}
}

func TestDownloadRejectsAShortBody(t *testing.T) {
	// An error page from a proxy or a truncated transfer is smaller than a real
	// helper, and installing it would give the node a file it cannot run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not a helper")
	}))
	defer srv.Close()

	dir := t.TempDir()
	r := &Resolver{}
	_, err := r.download(context.Background(), srv.URL+"/yt-dlp.exe", dir, source{tool: "yt-dlp"})
	if err == nil {
		t.Fatal("download accepted a body smaller than a helper")
	}
	if !strings.Contains(err.Error(), "returned only") {
		t.Fatalf("got %v, want it to say how little arrived", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("reading the download directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("left %d files behind for a rejected download", len(entries))
	}
}

// TestDownloadNamesTheFile covers what a downloaded archive is called, which is
// what the checksum listing and the in-run memo are keyed on.
func TestDownloadNamesTheFile(t *testing.T) {
	body := make([]byte, MinSize)
	for i := range body {
		body[i] = byte(i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	cases := []struct {
		name string
		url  string
		src  source
		want string
	}{
		{
			// The source names the file because the redirect the download
			// follows would otherwise name it something meaningless.
			name: "the source's asset name wins",
			url:  srv.URL + "/latest/download",
			src:  source{tool: "yt-dlp", asset: "yt-dlp.exe"},
			want: "yt-dlp.exe",
		},
		{
			name: "the URL names it when the source does not",
			url:  srv.URL + "/releases/yt-dlp_linux?download=1",
			src:  source{tool: "yt-dlp"},
			want: "yt-dlp_linux",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			r := &Resolver{}
			got, err := r.download(context.Background(), c.url, dir, c.src)
			if err != nil {
				t.Fatalf("download: %v", err)
			}
			if filepath.Base(got) != c.want {
				t.Fatalf("downloaded to %q, want the file to be called %q", got, c.want)
			}
			info, err := os.Stat(got)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if info.Size() != MinSize {
				t.Fatalf("kept %d bytes, want %d", info.Size(), MinSize)
			}
		})
	}
}

// TestFetchTriesEachURLInOrder covers the mirror list a plan can carry: the
// release channel has answered with an error for a target whose build was never
// published, and the snapshot beside it is what the download falls back to.
func TestFetchTriesEachURLInOrder(t *testing.T) {
	body := make([]byte, MinSize)
	var release, snapshot atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/release.zip") {
			release.Add(1)
			http.Error(w, "not published", http.StatusNotFound)
			return
		}
		snapshot.Add(1)
		w.Write(body)
	}))
	defer srv.Close()

	r := &Resolver{}
	t.Cleanup(func() { r.Close() })
	src := source{
		tool:  "ffmpeg",
		urls:  []string{srv.URL + "/release.zip", srv.URL + "/snapshot.zip"},
		asset: "ffmpeg.zip",
	}
	got, err := r.fetch(context.Background(), src)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if base := filepath.Base(got); base != "ffmpeg.zip" {
		t.Fatalf("saved the archive as %q, want the asset name", base)
	}
	if n := release.Load(); n != 1 {
		t.Fatalf("the failing URL was tried %d times, want once", n)
	}
	if n := snapshot.Load(); n != 1 {
		t.Fatalf("the fallback URL was tried %d times, want once", n)
	}
}

// TestFetchRemembersTheArchiveForLaterHelpers covers the memo: FFmpeg and
// ffprobe arrive in one archive, and a second helper served from it must not
// move the same bytes twice. Close then removes what the run fetched.
func TestFetchRemembersTheArchiveForLaterHelpers(t *testing.T) {
	body := make([]byte, MinSize)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(body)
	}))
	defer srv.Close()

	r := &Resolver{}
	src := source{tool: "ffmpeg", urls: []string{srv.URL + "/release.zip"}, asset: "ffmpeg.zip"}
	first, err := r.fetch(context.Background(), src)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	second, err := r.fetch(context.Background(), src)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if first != second {
		t.Fatalf("the archive was fetched to %q and then to %q", first, second)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the archive was transferred %d times, want once", n)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("the fetched archive was left behind: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("closing again: %v", err)
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "helper.zip")
	if err := os.WriteFile(file, []byte("the downloaded bytes"), 0o644); err != nil {
		t.Fatalf("writing the download: %v", err)
	}
	sum := sha256.Sum256([]byte("the downloaded bytes"))
	good := hex.EncodeToString(sum[:])

	serve := func(t *testing.T, listing string) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, listing)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("the published sum is accepted", func(t *testing.T) {
		srv := serve(t, good+"  helper.zip\n")
		if err := verify(context.Background(), srv.Client(), srv.URL+"/sums", "helper.zip", file); err != nil {
			t.Fatalf("verify: %v", err)
		}
	})

	t.Run("a different body is refused", func(t *testing.T) {
		// The whole point of the listing: the bytes that arrived are not the
		// bytes that were published.
		srv := serve(t, strings.Repeat("0", 64)+"  helper.zip\n")
		err := verify(context.Background(), srv.Client(), srv.URL+"/sums", "helper.zip", file)
		if err == nil {
			t.Fatal("verify accepted a file whose sum does not match")
		}
		if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("got %v, want a mismatch", err)
		}
	})

	t.Run("an asset the listing does not cover is refused", func(t *testing.T) {
		srv := serve(t, good+"  something-else.zip\n")
		err := verify(context.Background(), srv.Client(), srv.URL+"/sums", "helper.zip", file)
		if err == nil {
			t.Fatal("verify accepted an asset the listing does not mention")
		}
		if !strings.Contains(err.Error(), "no entry") {
			t.Fatalf("got %v, want it to say the listing has no entry", err)
		}
	})
}
