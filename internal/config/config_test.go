package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv removes the node's variables for the duration of the test and puts
// them back afterwards. Load reads the environment on every call, so without
// this a shell that exports GOLALINK_LOG_LEVEL would decide what a test meant
// to describe a config file actually observes.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "GOLALINK_") && !strings.HasPrefix(name, "SPOTIFY_") {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() { os.Setenv(name, value) })
	}
}

func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// validBaseline is Default() with the one field an operator must supply. Every
// Validate case below starts from it, so each case isolates a single rejection.
func validBaseline() Config {
	c := Default()
	c.Password = "secret"
	return c
}

func TestDefault(t *testing.T) {
	c := Default()
	for _, tc := range []struct {
		field     string
		got, want any
	}{
		{"address", c.Address, "127.0.0.1:2333"},
		{"logLevel", c.LogLevel, "info"},
		{"logFormat", c.LogFormat, "text"},
		{"logColor", c.LogColor, "auto"},
		{"logDirectory", c.LogDirectory, "logs"},
		{"inProcessAudio", c.InProcessAudio, true},
		{"ytDlpEnabled", c.YTDLPEnabled, true},
		{"youtubeNative", c.YoutubeNative, true},
		{"soundcloudNative", c.SoundCloudNative, true},
		{"lyricsSource", c.LyricsSource, "lrclib"},
		{"allowPrivateNetworks", c.AllowPrivateNetworks, false},
		{"webhook.enable", c.Webhook.Enable, false},
		{"webhook.stateLatencySeconds", c.Webhook.StateLatencySeconds, 5},
		{"ratelimits.enable", c.RateLimits.Enable, true},
		{"maxPlayers", c.MaxPlayers, 100},
		{"maxPlaylistTracks", c.MaxPlaylistTracks, 100},
		{"opusBitrate", c.OpusBitrate, 128000},
		{"cacheEntries", c.CacheEntries, 1024},
		{"loadTimeoutSeconds", c.LoadTimeoutSeconds, 30},
		{"soundcloudClientIdTtlSeconds", c.SoundCloudClientIDTTLSecond, 21600},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.field, tc.got, tc.want)
		}
	}

	// An address of 0.0.0.0 or a public one would expose an unauthenticated node
	// to the network the moment it starts, so the zero-config default must stay
	// on loopback.
	if c.Address != "127.0.0.1:2333" {
		t.Errorf("a node started with no config must listen on loopback only, got %q", c.Address)
	}
	if c.Filters != nil {
		t.Errorf("filters = %v, want nil so every filter stays available", c.Filters)
	}

	// Default cannot be started as-is: there is no password, and Validate must
	// say so rather than let an unauthenticated node serve clients.
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("Validate() = %v, want a missing-password error", err)
	}
	if c.Password != "" {
		t.Errorf("password = %q, want empty so operators are forced to set one", c.Password)
	}
}

func TestLoadYAMLFile(t *testing.T) {
	isolateEnv(t)
	path := writeConfig(t, "config.yaml", `
address: 0.0.0.0:3000
password: file-secret
logLevel: debug
logFormat: json
webhook:
  enable: true
  url: https://example.com/hook
  stateLatencySeconds: 0
filters:
  nightcore: false
maxPlayers: 7
`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		field     string
		got, want any
	}{
		{"address", c.Address, "0.0.0.0:3000"},
		{"password", c.Password, "file-secret"},
		{"logLevel", c.LogLevel, "debug"},
		{"logFormat", c.LogFormat, "json"},
		{"maxPlayers", c.MaxPlayers, 7},
		{"webhook.enable", c.Webhook.Enable, true},
		{"webhook.url", c.Webhook.URL, "https://example.com/hook"},
		{"filters.nightcore", c.Filters["nightcore"], false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.field, tc.got, tc.want)
		}
	}

	// The file is decoded onto Default(), not onto a zero Config: a key the
	// operator left out must keep its tuned default instead of collapsing to 0
	// and failing Validate.
	if c.MaxSessions != Default().MaxSessions || c.OpusBitrate != Default().OpusBitrate {
		t.Errorf("fields absent from the file lost their defaults: maxSessions=%d opusBitrate=%d", c.MaxSessions, c.OpusBitrate)
	}

	// An explicit zero in the file is a decision to switch the node-wide state
	// report off, and must not be confused with the absent key that means 5.
	if c.Webhook.StateLatencySeconds != 0 {
		t.Errorf("stateLatencySeconds = %d, want the explicit 0 from the file", c.Webhook.StateLatencySeconds)
	}
	if len(c.Filters) != 1 {
		t.Errorf("filters = %v, want only the entry the file lists", c.Filters)
	}
}

func TestLoadAcceptsJSON(t *testing.T) {
	isolateEnv(t)
	// Older deployments wrote JSON; YAML parses it, and that promise is part of
	// the format compatibility the decoder is chosen for.
	path := writeConfig(t, "config.json", `{"password":"json-secret","logLevel":"warn","maxSessions":42}`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Password != "json-secret" || c.LogLevel != "warn" || c.MaxSessions != 42 {
		t.Errorf("JSON config loaded as %+v", c)
	}
}

func TestLoadRejectsBadDocuments(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// KnownFields is on so a typo like "loglevel" fails loudly instead of
		// being silently ignored and leaving the default in place.
		{
			name: "misspelled key",
			body: "password: secret\nloglevel: debug\n",
			want: "field loglevel not found",
		},
		// A file with two documents would otherwise load only the first and
		// drop the rest without a word.
		{
			name: "two documents",
			body: "password: one\n---\npassword: two\n",
			want: "one YAML document",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			_, err := Load(writeConfig(t, "config.yaml", tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadWithoutFile(t *testing.T) {
	t.Run("missing path is reported", func(t *testing.T) {
		isolateEnv(t)
		c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil {
			t.Fatal("Load of a missing file succeeded")
		}
		// The returned config stays usable so a caller can report the failure
		// alongside the settings it would have run with.
		if c.Address != Default().Address {
			t.Errorf("address = %q, want the default alongside the error", c.Address)
		}
	})

	// An empty file plus GOLALINK_PASSWORD is a supported deployment: an
	// operator may keep everything in the environment, and a blank config must
	// not be treated as corrupt.
	t.Run("empty file keeps defaults", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("GOLALINK_PASSWORD", "env-secret")
		c, err := Load(writeConfig(t, "config.yaml", ""))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Password != "env-secret" || c.MaxPlayers != Default().MaxPlayers {
			t.Errorf("loaded %+v, want defaults with the environment password", c)
		}
	})

	t.Run("no path reads only the environment", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("GOLALINK_PASSWORD", "env-secret")
		t.Setenv("GOLALINK_ADDRESS", "127.0.0.1:9999")
		c, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Address != "127.0.0.1:9999" {
			t.Errorf("address = %q, want the environment value", c.Address)
		}
	})
}

func TestEnvironmentOverridesFile(t *testing.T) {
	isolateEnv(t)
	path := writeConfig(t, "config.yaml", "address: 10.0.0.1:2333\npassword: file-secret\nmaxPlayers: 3\n")
	t.Setenv("GOLALINK_ADDRESS", "127.0.0.1:4000")
	t.Setenv("GOLALINK_MAX_PLAYERS", "") // not a key Load reads; must not matter

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Containers override the baked-in file, so the environment has to win — and
	// only for the keys it names.
	if c.Address != "127.0.0.1:4000" {
		t.Errorf("address = %q, want the environment value to win over the file", c.Address)
	}
	if c.Password != "file-secret" || c.MaxPlayers != 3 {
		t.Errorf("load = %+v, want untouched file values for keys the environment does not name", c)
	}
}

func TestStringEnvironmentOverrides(t *testing.T) {
	read := map[string]func(Config) string{
		"GOLALINK_ADDRESS":                 func(c Config) string { return c.Address },
		"GOLALINK_LOG_LEVEL":               func(c Config) string { return c.LogLevel },
		"GOLALINK_LOG_FORMAT":              func(c Config) string { return c.LogFormat },
		"GOLALINK_LOG_DIRECTORY":           func(c Config) string { return c.LogDirectory },
		"GOLALINK_LOG_COLOR":               func(c Config) string { return c.LogColor },
		"GOLALINK_LYRICS_SOURCE":           func(c Config) string { return c.LyricsSource },
		"GOLALINK_TOOLS_DIRECTORY":         func(c Config) string { return c.ToolsDirectory },
		"GOLALINK_WEBHOOK_URL":             func(c Config) string { return c.Webhook.URL },
		"GOLALINK_YOUTUBE_ENDPOINT":        func(c Config) string { return c.YoutubeEndpoint },
		"GOLALINK_SOUNDCLOUD_CLIENT_ID":    func(c Config) string { return c.SoundCloudClientID },
		"SPOTIFY_MARKET":                   func(c Config) string { return c.SpotifyMarket },
		"SPOTIFY_CLIENT_ID":                func(c Config) string { return c.SpotifyClientID },
		"GOLALINK_SOUNDCLOUD_WEB_ENDPOINT": func(c Config) string { return c.SoundCloudWebEndpoint },
	}

	cases := []struct {
		key   string
		value string
		want  string
	}{
		{"GOLALINK_ADDRESS", "127.0.0.1:3001", "127.0.0.1:3001"},
		{"GOLALINK_LOG_LEVEL", "error", "error"},
		{"GOLALINK_LOG_FORMAT", "json", "json"},
		// An empty value that is present is a setting, not an omission: this is
		// how a console-only deployment switches the daily log files off.
		{"GOLALINK_LOG_DIRECTORY", "", ""},
		{"GOLALINK_LOG_COLOR", "never", "never"},
		{"GOLALINK_LYRICS_SOURCE", "none", "none"},
		{"GOLALINK_TOOLS_DIRECTORY", "/opt/golalink/tools", "/opt/golalink/tools"},
		{"GOLALINK_WEBHOOK_URL", "https://example.com/hook", "https://example.com/hook"},
		{"GOLALINK_YOUTUBE_ENDPOINT", "https://youtube.example", "https://youtube.example"},
		{"GOLALINK_SOUNDCLOUD_CLIENT_ID", "abc123", "abc123"},
		// Spotify's variables deliberately keep their vendor prefix so an
		// existing .env from the reference implementation still works.
		{"SPOTIFY_MARKET", "GB", "GB"},
		{"SPOTIFY_CLIENT_ID", "client-id", "client-id"},
		{"GOLALINK_SOUNDCLOUD_WEB_ENDPOINT", "https://soundcloud.example", "https://soundcloud.example"},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			isolateEnv(t)
			get := read[tc.key]
			if get == nil {
				t.Fatalf("no accessor for %s", tc.key)
			}
			t.Setenv("GOLALINK_PASSWORD", "secret")
			// A webhook URL alone must not enable the webhook, and a Spotify id
			// alone would fail Validate, so pair the dependent keys.
			if tc.key == "GOLALINK_WEBHOOK_URL" {
				t.Setenv("GOLALINK_WEBHOOK_ENABLED", "0")
			}
			if tc.key == "SPOTIFY_CLIENT_ID" {
				t.Setenv("SPOTIFY_CLIENT_SECRET", "client-secret")
			}
			t.Setenv(tc.key, tc.value)

			c, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := get(c); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestBooleanEnvironmentOverrides(t *testing.T) {
	read := map[string]func(Config) bool{
		"GOLALINK_YOUTUBE_NATIVE":    func(c Config) bool { return c.YoutubeNative },
		"GOLALINK_SOUNDCLOUD":        func(c Config) bool { return c.SoundCloudNative },
		"GOLALINK_YTDLP_ENABLED":     func(c Config) bool { return c.YTDLPEnabled },
		"GOLALINK_AUTO_DOWNLOAD":     func(c Config) bool { return c.AutoDownload },
		"GOLALINK_IN_PROCESS_AUDIO":  func(c Config) bool { return c.InProcessAudio },
		"GOLALINK_PRIVATE_NETWORKS":  func(c Config) bool { return c.AllowPrivateNetworks },
		"GOLALINK_RATELIMITS_ENABLE": func(c Config) bool { return c.RateLimits.Enable },
		"GOLALINK_WEBHOOK_ENABLED":   func(c Config) bool { return c.Webhook.Enable },
	}

	cases := []struct {
		key   string
		value string
		want  bool
	}{
		{"GOLALINK_YOUTUBE_NATIVE", "0", false},
		{"GOLALINK_YOUTUBE_NATIVE", "false", false},
		{"GOLALINK_YOUTUBE_NATIVE", "FALSE", false},
		{"GOLALINK_YOUTUBE_NATIVE", "1", true},
		{"GOLALINK_YOUTUBE_NATIVE", "true", true},
		// Only an explicit 0/false disables a switch; anything else present is
		// read as on, which is what lets `GOLALINK_X=yes` in a compose file do
		// what its author expected.
		{"GOLALINK_YOUTUBE_NATIVE", "no", true},
		{"GOLALINK_YOUTUBE_NATIVE", "", true},
		{"GOLALINK_SOUNDCLOUD", "0", false},
		{"GOLALINK_YTDLP_ENABLED", "false", false},
		{"GOLALINK_AUTO_DOWNLOAD", "1", true},
		{"GOLALINK_IN_PROCESS_AUDIO", "0", false},
		{"GOLALINK_PRIVATE_NETWORKS", "true", true},
		{"GOLALINK_RATELIMITS_ENABLE", "0", false},
		{"GOLALINK_WEBHOOK_ENABLED", "0", false},
	}

	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			isolateEnv(t)
			get := read[tc.key]
			if get == nil {
				t.Fatalf("no accessor for %s", tc.key)
			}
			t.Setenv("GOLALINK_PASSWORD", "secret")
			t.Setenv(tc.key, tc.value)

			c, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := get(c); got != tc.want {
				t.Errorf("%s=%q gives %v, want %v", tc.key, tc.value, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // empty means the config is accepted
	}{
		{"baseline is accepted", func(*Config) {}, ""},
		{"no password", func(c *Config) { c.Password = "" }, "password"},
		{"no address", func(c *Config) { c.Address = "" }, "address is required"},
		// Auto-download writes binaries, so its destination cannot be blank.
		{"autoDownload without a directory", func(c *Config) { c.AutoDownload = true; c.ToolsDirectory = "" }, "toolsDirectory is required"},
		{"unknown log level", func(c *Config) { c.LogLevel = "verbose" }, "logLevel must be one of"},
		// Case and padding are forgiven, so a hand-edited file with "INFO " is
		// not a startup failure.
		{"log level padded and upcased", func(c *Config) { c.LogLevel = "  WARN " }, ""},
		{"unknown log format", func(c *Config) { c.LogFormat = "logfmt" }, "logFormat must be one of"},
		{"unknown log colour", func(c *Config) { c.LogColor = "sometimes" }, "logColor must be one of"},
		{"unknown lyrics source", func(c *Config) { c.LyricsSource = "genius" }, "lyricsSource must be one of"},
		{"webhook enabled with a blank url", func(c *Config) { c.Webhook.Enable = true; c.Webhook.URL = "   " }, "webhook.url is required"},
		// A webhook is an outbound POST: a file:// or ftp:// target would fail
		// every event at runtime, so the scheme is checked at startup.
		{"webhook url with ftp", func(c *Config) { c.Webhook.Enable = true; c.Webhook.URL = "ftp://example.com/hook" }, "http or https"},
		{"webhook url with a bare scheme", func(c *Config) { c.Webhook.Enable = true; c.Webhook.URL = "https://" }, "http or https"},
		{"webhook url padded", func(c *Config) { c.Webhook.Enable = true; c.Webhook.URL = "  https://example.com/hook  " }, ""},
		// Zero switches the node-wide state report off; that is a decision the
		// node must accept, while a negative latency is nonsense.
		{"webhook latency of zero", func(c *Config) { c.Webhook.StateLatencySeconds = 0 }, ""},
		{"webhook latency negative", func(c *Config) { c.Webhook.StateLatencySeconds = -1 }, "between 0 and 100000"},
		{"webhook latency above the cap", func(c *Config) { c.Webhook.StateLatencySeconds = 100001 }, "between 0 and 100000"},
		{"webhook disabled keeps a blank url", func(c *Config) { c.Webhook.Enable = false; c.Webhook.URL = "" }, ""},
		{"native youtube needs both endpoints", func(c *Config) { c.YoutubeMusicEndpoint = "" }, "youtubeEndpoint"},
		// Turning the native client off is the way to prove a deployment never
		// talks to YouTube directly, so the endpoints stop being required.
		{"native youtube off allows blank endpoints", func(c *Config) { c.YoutubeNative = false; c.YoutubeEndpoint = ""; c.YoutubeMusicEndpoint = "" }, ""},
		{"native soundcloud needs both endpoints", func(c *Config) { c.SoundCloudAPIEndpoint = "" }, "soundcloudApiEndpoint"},
		{"zero maxPlayers", func(c *Config) { c.MaxPlayers = 0 }, "maxPlayers must be between"},
		{"playlist cap above the range", func(c *Config) { c.MaxPlaylistTracks = 100001 }, "maxPlaylistTracks must be between"},
		{"zero event buffer", func(c *Config) { c.EventBuffer = 0 }, "eventBuffer must be between"},
		{"opus bitrate below the floor", func(c *Config) { c.OpusBitrate = 8000 }, "opusBitrate"},
		{"opus bitrate above the ceiling", func(c *Config) { c.OpusBitrate = 512000 }, "opusBitrate"},
		{"spotify id without secret", func(c *Config) { c.SpotifyClientID = "id" }, "both be set"},
		{"spotify refresh token without an app", func(c *Config) { c.SpotifyRefreshToken = "token" }, "requires the application client ID"},
		{"spotify client without a market", func(c *Config) {
			c.SpotifyClientID = "id"
			c.SpotifyClientSecret = "secret"
			c.SpotifyMarket = ""
		}, "spotifyMarket is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validBaseline()
			tc.mutate(&c)
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want the config accepted", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("Validate() accepted the config, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
