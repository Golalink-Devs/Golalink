package tools

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	btbnBase  = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/"
	riedlBase = "https://ffmpeg.martin-riedl.de/redirect/latest/"
	ytdlpBase = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
)

// platform is the key the tables use: the asset a helper is downloaded from is
// chosen from runtime.GOOS and runtime.GOARCH, which a test can only observe for
// the platform it runs on. Every platform is written down, so the expectations
// are stated once and the run checks the one it can.
func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func TestFFmpegSourcePerPlatform(t *testing.T) {
	cases := map[string]struct {
		want    source
		wantErr bool
	}{
		"windows/amd64": {want: source{
			tool: "ffmpeg",
			urls: []string{btbnBase + "ffmpeg-master-latest-win64-gpl.zip"},
			// The listing is published beside the archive, keyed on the same
			// file name, so the two must not drift apart.
			sums:   btbnBase + "checksums.sha256",
			asset:  "ffmpeg-master-latest-win64-gpl.zip",
			member: "ffmpeg.exe",
		}},
		"windows/arm64": {want: source{
			tool:   "ffmpeg",
			urls:   []string{btbnBase + "ffmpeg-master-latest-winarm64-gpl.zip"},
			sums:   btbnBase + "checksums.sha256",
			asset:  "ffmpeg-master-latest-winarm64-gpl.zip",
			member: "ffmpeg.exe",
		}},
		"linux/amd64": {want: source{
			tool: "ffmpeg",
			// The snapshot channel is the fallback for a target whose release
			// build has not been published yet.
			urls:   []string{riedlBase + "linux/amd64/release/ffmpeg.zip", riedlBase + "linux/amd64/snapshot/ffmpeg.zip"},
			member: "ffmpeg",
		}},
		"linux/arm64": {want: source{
			tool:   "ffmpeg",
			urls:   []string{riedlBase + "linux/arm64/release/ffmpeg.zip", riedlBase + "linux/arm64/snapshot/ffmpeg.zip"},
			member: "ffmpeg",
		}},
		"darwin/amd64": {want: source{
			tool:   "ffmpeg",
			urls:   []string{riedlBase + "macos/amd64/release/ffmpeg.zip", riedlBase + "macos/amd64/snapshot/ffmpeg.zip"},
			member: "ffmpeg",
		}},
		"darwin/arm64": {want: source{
			tool:   "ffmpeg",
			urls:   []string{riedlBase + "macos/arm64/release/ffmpeg.zip", riedlBase + "macos/arm64/snapshot/ffmpeg.zip"},
			member: "ffmpeg",
		}},
		// A 32-bit build is not published, and asking for one must fail at the
		// plan rather than at a 404 during a download.
		"windows/386": {wantErr: true},
		"linux/386":   {wantErr: true},
	}

	c, ok := cases[platform()]
	if !ok {
		t.Skipf("no FFmpeg expectation for %s", platform())
	}
	got, err := sourceFor("ffmpeg")
	if c.wantErr {
		if err == nil {
			t.Fatalf("got %+v, want an error for an unpublished architecture", got)
		}
		if !strings.Contains(err.Error(), "no FFmpeg build") {
			t.Fatalf("got %v, want it to name the missing build", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("sourceFor: %v", err)
	}
	if !reflect.DeepEqual(got, c.want) {
		t.Fatalf("got %+v, want %+v", got, c.want)
	}
}

func TestYTDLPSourcePerPlatform(t *testing.T) {
	musl := isMusl()
	linuxAMD64 := "yt-dlp_linux"
	linuxARM64 := "yt-dlp_linux_aarch64"
	if musl {
		linuxAMD64, linuxARM64 = "yt-dlp_musllinux", "yt-dlp_musllinux_aarch64"
	}
	cases := map[string]string{
		"windows/amd64": "yt-dlp.exe",
		"windows/arm64": "yt-dlp_arm64.exe",
		"windows/386":   "yt-dlp_x86.exe",
		// One macOS binary covers both architectures.
		"darwin/amd64": "yt-dlp_macos",
		"darwin/arm64": "yt-dlp_macos",
		"linux/amd64":  linuxAMD64,
		"linux/arm64":  linuxARM64,
		"linux/386":    "",
	}

	asset, ok := cases[platform()]
	if !ok {
		t.Skipf("no yt-dlp expectation for %s", platform())
	}
	got, err := sourceFor("yt-dlp")
	if asset == "" {
		if err == nil {
			t.Fatalf("got %+v, want an error for an unpublished architecture", got)
		}
		return
	}
	if err != nil {
		t.Fatalf("sourceFor: %v", err)
	}
	want := source{
		tool:  "yt-dlp",
		urls:  []string{ytdlpBase + asset},
		sums:  ytdlpBase + "SHA2-256SUMS",
		asset: asset,
		// The release binaries are single files, so unpacking one as an archive
		// would fail; the empty member is what says to install it as it is.
		member: "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSourceForUnknownHelper(t *testing.T) {
	// A name the configuration made up, and the words a setting might use to
	// mean "no helper", must both be reported rather than resolved to a guess.
	for _, name := range []string{"none", "ffprobe", ""} {
		t.Run(name, func(t *testing.T) {
			if _, err := sourceFor(name); err == nil {
				t.Fatal("got a source, want an error")
			} else if !strings.Contains(err.Error(), "no download source") {
				t.Fatalf("got %v, want it to say no source is known", err)
			}
		})
	}
}

func TestExecutableName(t *testing.T) {
	want := "yt-dlp"
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got := executableName("yt-dlp"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
