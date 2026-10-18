package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// source describes where one helper comes from on the current platform.
type source struct {
	// tool is the helper name, used in progress logs.
	tool string
	// urls are tried in order; the first that answers is used.
	urls []string
	// sums is a "<sha256>  <file>" listing covering asset, or empty when the
	// provider publishes none.
	sums string
	// asset is the file name the listing and the cache are keyed on.
	asset string
	// member is the base name to pull out of a zip archive. Empty means the
	// download is the executable itself.
	member string
}

// sourceFor returns the download source for a helper on this platform.
func sourceFor(name string) (source, error) {
	switch name {
	case "ffmpeg":
		return ffmpegSource(name)
	case "yt-dlp":
		return ytdlpSource()
	}
	return source{}, fmt.Errorf("no download source is known for %s", name)
}

// ffmpegSource uses BtbN's Windows builds and martin-riedl.de for Linux and
// macOS. Both are GPL builds with libopus, which the encoder needs. The archive
// also holds ffprobe, which Golalink does not use: containers are parsed in
// process, so only the encoder is unpacked.
func ffmpegSource(name string) (source, error) {
	switch runtime.GOOS {
	case "windows":
		arch, err := windowsArch()
		if err != nil {
			return source{}, err
		}
		asset := "ffmpeg-master-latest-" + arch + "-gpl.zip"
		const base = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/"
		return source{
			tool:   name,
			urls:   []string{base + asset},
			sums:   base + "checksums.sha256",
			asset:  asset,
			member: name + ".exe",
		}, nil
	case "linux", "darwin":
		arch, err := posixArch()
		if err != nil {
			return source{}, err
		}
		// A release build is preferred; the snapshot channel covers targets
		// whose latest release has not been built yet.
		base := "https://ffmpeg.martin-riedl.de/redirect/latest/" + providerOS() + "/" + arch + "/"
		return source{
			tool:   name,
			urls:   []string{base + "release/" + name + ".zip", base + "snapshot/" + name + ".zip"},
			member: name,
		}, nil
	}
	return source{}, fmt.Errorf("no FFmpeg build is known for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// ytdlpSource uses the single-file release binaries, which need no Python.
func ytdlpSource() (source, error) {
	asset, err := ytdlpAsset()
	if err != nil {
		return source{}, err
	}
	const base = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	return source{
		tool:  "yt-dlp",
		urls:  []string{base + asset},
		sums:  base + "SHA2-256SUMS",
		asset: asset,
	}, nil
}

func ytdlpAsset() (string, error) {
	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return "yt-dlp.exe", nil
		case "arm64":
			return "yt-dlp_arm64.exe", nil
		case "386":
			return "yt-dlp_x86.exe", nil
		}
	case "darwin":
		return "yt-dlp_macos", nil
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			if isMusl() {
				return "yt-dlp_musllinux", nil
			}
			return "yt-dlp_linux", nil
		case "arm64":
			if isMusl() {
				return "yt-dlp_musllinux_aarch64", nil
			}
			return "yt-dlp_linux_aarch64", nil
		}
	}
	return "", fmt.Errorf("no yt-dlp build is known for %s/%s", runtime.GOOS, runtime.GOARCH)
}

func windowsArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "win64", nil
	case "arm64":
		return "winarm64", nil
	}
	return "", fmt.Errorf("no FFmpeg build is known for windows/%s", runtime.GOARCH)
}

func posixArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", nil
	case "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("no FFmpeg build is known for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// providerOS names the platform the way martin-riedl.de does.
func providerOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// isMusl reports whether the Linux binary runs against musl, which needs its own
// yt-dlp build.
func isMusl() bool {
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return true
	}
	matches, err := filepath.Glob("/lib/ld-musl-*.so.1")
	return err == nil && len(matches) > 0
}
