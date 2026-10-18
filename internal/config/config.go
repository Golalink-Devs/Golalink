// Package config loads the node configuration. Files are YAML so they can carry
// comments; JSON is accepted too, because it is a subset of YAML.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds every tunable of the node. Both tags are kept so the same names
// work in YAML files and in the JSON that older deployments already use.
type Config struct {
	Address  string `json:"address" yaml:"address"`
	Password string `json:"password" yaml:"password"`

	FFmpeg string `json:"ffmpeg" yaml:"ffmpeg"`

	// ToolsDirectory holds the media helpers downloaded at startup, relative to
	// the working directory.
	ToolsDirectory string `json:"toolsDirectory" yaml:"toolsDirectory"`
	// AutoDownload installs a media helper that is neither on PATH nor already
	// in ToolsDirectory.
	AutoDownload bool `json:"autoDownload" yaml:"autoDownload"`

	// InProcessAudio plays formats that have a decoder built in, instead of
	// decoding them with FFmpeg. FFmpeg still handles every format the built-in
	// decoders do not cover, so turning this off only removes an optimisation.
	InProcessAudio bool `json:"inProcessAudio" yaml:"inProcessAudio"`

	// LyricsSource names the service lyrics are fetched from. A name of none
	// switches lyrics off and stops the node calling anything for them.
	LyricsSource string `json:"lyricsSource" yaml:"lyricsSource"`

	LogLevel  string `json:"logLevel" yaml:"logLevel"`
	LogFormat string `json:"logFormat" yaml:"logFormat"`
	// LogDirectory mirrors the log into daily files inside this directory,
	// relative to the working directory. It defaults to "logs"; set it to an
	// empty string to keep the console as the only output.
	LogDirectory string `json:"logDirectory" yaml:"logDirectory"`
	// LogColor is auto, always or never. Auto colours a terminal and leaves a
	// redirected stream plain.
	LogColor string `json:"logColor" yaml:"logColor"`

	// YTDLP is only a fallback: YouTube and SoundCloud are served natively.
	// Set YTDLPEnabled to false to prove a deployment never shells out.
	YTDLP        string `json:"ytDlp" yaml:"ytDlp"`
	YTDLPEnabled bool   `json:"ytDlpEnabled" yaml:"ytDlpEnabled"`

	YoutubeNative        bool   `json:"youtubeNative" yaml:"youtubeNative"`
	YoutubeEndpoint      string `json:"youtubeEndpoint" yaml:"youtubeEndpoint"`
	YoutubeMusicEndpoint string `json:"youtubeMusicEndpoint" yaml:"youtubeMusicEndpoint"`
	YoutubeClientVersion string `json:"youtubeClientVersion" yaml:"youtubeClientVersion"`

	SoundCloudNative            bool   `json:"soundcloudNative" yaml:"soundcloudNative"`
	SoundCloudAPIEndpoint       string `json:"soundcloudApiEndpoint" yaml:"soundcloudApiEndpoint"`
	SoundCloudWebEndpoint       string `json:"soundcloudWebEndpoint" yaml:"soundcloudWebEndpoint"`
	SoundCloudClientID          string `json:"soundcloudClientId" yaml:"soundcloudClientId"`
	SoundCloudClientIDTTLSecond int    `json:"soundcloudClientIdTtlSeconds" yaml:"soundcloudClientIdTtlSeconds"`

	SpotifyClientID     string `json:"spotifyClientId" yaml:"spotifyClientId"`
	SpotifyClientSecret string `json:"spotifyClientSecret" yaml:"spotifyClientSecret"`
	SpotifyRefreshToken string `json:"spotifyRefreshToken" yaml:"spotifyRefreshToken"`
	SpotifyMarket       string `json:"spotifyMarket" yaml:"spotifyMarket"`

	DeezerAPIEndpoint string `json:"deezerApiEndpoint" yaml:"deezerApiEndpoint"`

	// Filters maps a filter name to whether clients may use it, the way the
	// reference implementation's config does. A filter that is absent from the
	// map stays enabled; listing it as false makes PATCH player refuse it.
	Filters map[string]bool `json:"filters" yaml:"filters"`

	AllowPrivateNetworks bool `json:"allowPrivateNetworks" yaml:"allowPrivateNetworks"`

	MaxPlayers         int `json:"maxPlayers" yaml:"maxPlayers"`
	MaxSessions        int `json:"maxSessions" yaml:"maxSessions"`
	MaxConcurrentLoads int `json:"maxConcurrentLoads" yaml:"maxConcurrentLoads"`
	MaxPlaylistTracks  int `json:"maxPlaylistTracks" yaml:"maxPlaylistTracks"`
	SearchResults      int `json:"searchResults" yaml:"searchResults"`

	CacheEntries    int `json:"cacheEntries" yaml:"cacheEntries"`
	CacheTTLSeconds int `json:"cacheTtlSeconds" yaml:"cacheTtlSeconds"`

	LoadTimeoutSeconds  int `json:"loadTimeoutSeconds" yaml:"loadTimeoutSeconds"`
	PlayerUpdateSeconds int `json:"playerUpdateSeconds" yaml:"playerUpdateSeconds"`
	StatsSeconds        int `json:"statsSeconds" yaml:"statsSeconds"`
	EventBuffer         int `json:"eventBuffer" yaml:"eventBuffer"`
	OpusBitrate         int `json:"opusBitrate" yaml:"opusBitrate"`

	// Ratelimiter
	RateLimits RateLimit `json:"ratelimits" yaml:"ratelimits"`

	// Webhook posts node events to an external endpoint.
	Webhook Webhook `json:"webhook" yaml:"webhook"`
}

// RateLimit holds the request throttling switches.
type RateLimit struct {
	Enable bool `json:"enable" yaml:"enable"`
}

// Webhook describes where node events are posted. An endpoint that is a Discord
// webhook is sent an embed; any other endpoint is sent the event itself.
type Webhook struct {
	Enable bool   `json:"enable" yaml:"enable"`
	URL    string `json:"url" yaml:"url"`
	// StateLatencySeconds is how long the node waits between reports on the
	// state of every player. Clients keep receiving their own playerUpdate; this
	// is the node-wide summary, so the interval is a latency rather than a tick.
	// Zero turns the report off, leaving the endpoint carrying only events.
	StateLatencySeconds int `json:"stateLatencySeconds" yaml:"stateLatencySeconds"`
}

// LogLevels, LogFormats and LogColors are the accepted values for the logging
// settings.
var (
	LogLevels  = []string{"debug", "info", "warn", "error"}
	LogFormats = []string{"text", "json"}
	LogColors  = []string{"auto", "always", "never"}
	// LyricsSources are the accepted values for the lyrics setting.
	LyricsSources = []string{"lrclib", "none"}
)

func Default() Config {
	return Config{
		Address:                     "127.0.0.1:2333",
		FFmpeg:                      "ffmpeg",
		ToolsDirectory:              ".tools",
		AutoDownload:                false,
		InProcessAudio:              true,
		LyricsSource:                "lrclib",
		LogLevel:                    "info",
		LogFormat:                   "text",
		LogColor:                    "auto",
		LogDirectory:                "logs",
		YTDLP:                       "yt-dlp",
		YTDLPEnabled:                true,
		YoutubeNative:               true,
		YoutubeEndpoint:             "https://youtubei.googleapis.com",
		YoutubeMusicEndpoint:        "https://music.youtube.com",
		SoundCloudNative:            true,
		SoundCloudAPIEndpoint:       "https://api-v2.soundcloud.com",
		SoundCloudWebEndpoint:       "https://soundcloud.com",
		SoundCloudClientIDTTLSecond: 21600,
		SpotifyMarket:               "US",
		DeezerAPIEndpoint:           "https://api.deezer.com",
		MaxPlayers:                  100,
		MaxSessions:                 100,
		MaxConcurrentLoads:          8,
		MaxPlaylistTracks:           100,
		SearchResults:               5,
		CacheEntries:                1024,
		CacheTTLSeconds:             300,
		LoadTimeoutSeconds:          30,
		PlayerUpdateSeconds:         5,
		StatsSeconds:                60,
		EventBuffer:                 256,
		Webhook:                     Webhook{StateLatencySeconds: 5},
		OpusBitrate:                 128000,
		RateLimits:                  RateLimit{Enable: true},
	}
}

// Candidates are the file names Discover looks for, in order.
var Candidates = []string{"config.yaml", "config.yml", "config.json"}

// Discover returns the configuration file to load. An explicit path is used as
// given; otherwise the first conventional name present in the working directory
// is used, so an unpacked release starts with no arguments. An empty result means
// there is no file and the defaults plus the environment decide everything.
func Discover(path string) string {
	if path != "" {
		return path
	}
	for _, name := range Candidates {
		if info, err := os.Stat(name); err == nil && !info.IsDir() {
			return name
		}
	}
	return ""
}

func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return c, err
		}
		defer f.Close()
		// YAML also parses JSON, so an existing config.json keeps working.
		d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
		d.KnownFields(true)
		if err = d.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return c, err
		}
		var trailing any
		if err = d.Decode(&trailing); !errors.Is(err, io.EOF) {
			return c, errors.New("configuration must contain one YAML document")
		}
	}
	for key, dest := range map[string]*string{
		"GOLALINK_ADDRESS":                 &c.Address,
		"GOLALINK_PASSWORD":                &c.Password,
		"GOLALINK_FFMPEG":                  &c.FFmpeg,
		"GOLALINK_TOOLS_DIRECTORY":         &c.ToolsDirectory,
		"GOLALINK_LOG_LEVEL":               &c.LogLevel,
		"GOLALINK_LOG_FORMAT":              &c.LogFormat,
		"GOLALINK_LOG_DIRECTORY":           &c.LogDirectory,
		"GOLALINK_LOG_COLOR":               &c.LogColor,
		"GOLALINK_YTDLP":                   &c.YTDLP,
		"GOLALINK_YOUTUBE_ENDPOINT":        &c.YoutubeEndpoint,
		"GOLALINK_YOUTUBE_MUSIC_ENDPOINT":  &c.YoutubeMusicEndpoint,
		"GOLALINK_YOUTUBE_CLIENT_VERSION":  &c.YoutubeClientVersion,
		"GOLALINK_SOUNDCLOUD_API_ENDPOINT": &c.SoundCloudAPIEndpoint,
		"GOLALINK_SOUNDCLOUD_WEB_ENDPOINT": &c.SoundCloudWebEndpoint,
		"GOLALINK_SOUNDCLOUD_CLIENT_ID":    &c.SoundCloudClientID,
		"GOLALINK_DEEZER_API_ENDPOINT":     &c.DeezerAPIEndpoint,
		"SPOTIFY_CLIENT_ID":                &c.SpotifyClientID,
		"SPOTIFY_CLIENT_SECRET":            &c.SpotifyClientSecret,
		"SPOTIFY_REFRESH_TOKEN":            &c.SpotifyRefreshToken,
		"SPOTIFY_MARKET":                   &c.SpotifyMarket,
		"GOLALINK_LYRICS_SOURCE":           &c.LyricsSource,
		"GOLALINK_WEBHOOK_URL":             &c.Webhook.URL,
	} {
		if v, ok := os.LookupEnv(key); ok {
			*dest = v
		}
	}
	for key, dest := range map[string]*bool{
		"GOLALINK_YOUTUBE_NATIVE":    &c.YoutubeNative,
		"GOLALINK_SOUNDCLOUD":        &c.SoundCloudNative,
		"GOLALINK_YTDLP_ENABLED":     &c.YTDLPEnabled,
		"GOLALINK_AUTO_DOWNLOAD":     &c.AutoDownload,
		"GOLALINK_IN_PROCESS_AUDIO":  &c.InProcessAudio,
		"GOLALINK_PRIVATE_NETWORKS":  &c.AllowPrivateNetworks,
		"GOLALINK_RATELIMITS_ENABLE": &c.RateLimits.Enable,
		"GOLALINK_WEBHOOK_ENABLED":   &c.Webhook.Enable,
	} {
		if v, ok := os.LookupEnv(key); ok {
			*dest = !(v == "0" || strings.EqualFold(v, "false"))
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.Password == "" {
		return errors.New("set GOLALINK_PASSWORD or password in config")
	}
	if c.Address == "" {
		return errors.New("address is required")
	}
	if c.AutoDownload && c.ToolsDirectory == "" {
		return errors.New("toolsDirectory is required while autoDownload is on")
	}
	if !oneOf(c.LyricsSource, LyricsSources) {
		return fmt.Errorf("lyricsSource must be one of %s", strings.Join(LyricsSources, ", "))
	}
	if !oneOf(c.LogLevel, LogLevels) {
		return fmt.Errorf("logLevel must be one of %s", strings.Join(LogLevels, ", "))
	}
	if !oneOf(c.LogFormat, LogFormats) {
		return fmt.Errorf("logFormat must be one of %s", strings.Join(LogFormats, ", "))
	}
	if !oneOf(c.LogColor, LogColors) {
		return fmt.Errorf("logColor must be one of %s", strings.Join(LogColors, ", "))
	}
	if c.Webhook.Enable && strings.TrimSpace(c.Webhook.URL) == "" {
		return errors.New("webhook.url is required while webhook is enabled")
	}
	if c.Webhook.StateLatencySeconds < 0 || c.Webhook.StateLatencySeconds > 100000 {
		return errors.New("webhook.stateLatencySeconds must be between 0 and 100000")
	}
	if u := strings.TrimSpace(c.Webhook.URL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("webhook.url must be an http or https URL")
		}
	}
	if c.YoutubeNative && (c.YoutubeEndpoint == "" || c.YoutubeMusicEndpoint == "") {
		return errors.New("youtubeEndpoint and youtubeMusicEndpoint are required while youtubeNative is on")
	}
	if c.SoundCloudNative && (c.SoundCloudAPIEndpoint == "" || c.SoundCloudWebEndpoint == "") {
		return errors.New("soundcloudApiEndpoint and soundcloudWebEndpoint are required while soundcloudNative is on")
	}
	for k, v := range map[string]int{"maxPlayers": c.MaxPlayers, "maxSessions": c.MaxSessions, "maxConcurrentLoads": c.MaxConcurrentLoads, "maxPlaylistTracks": c.MaxPlaylistTracks, "searchResults": c.SearchResults, "cacheEntries": c.CacheEntries, "cacheTtlSeconds": c.CacheTTLSeconds, "loadTimeoutSeconds": c.LoadTimeoutSeconds, "playerUpdateSeconds": c.PlayerUpdateSeconds, "statsSeconds": c.StatsSeconds, "eventBuffer": c.EventBuffer, "soundcloudClientIdTtlSeconds": c.SoundCloudClientIDTTLSecond} {
		if v < 1 || v > 100000 {
			return fmt.Errorf("%s must be between 1 and 100000", k)
		}
	}
	if c.OpusBitrate < 16000 || c.OpusBitrate > 256000 {
		return errors.New("opusBitrate must be 16000..256000")
	}
	if (c.SpotifyClientID == "") != (c.SpotifyClientSecret == "") {
		return errors.New("Spotify client ID and secret must both be set")
	}
	if c.SpotifyRefreshToken != "" && c.SpotifyClientID == "" {
		return errors.New("Spotify refresh token requires the application client ID and secret")
	}
	if c.SpotifyClientID != "" && c.SpotifyMarket == "" {
		return errors.New("spotifyMarket is required when Spotify credentials are set")
	}
	return nil
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(v), a) {
			return true
		}
	}
	return false
}

func (c Config) LoadTimeout() time.Duration { return time.Duration(c.LoadTimeoutSeconds) * time.Second }

// SoundCloudClientIDTTL is how long a scraped SoundCloud client id is reused
// before it is fetched again.
func (c Config) SoundCloudClientIDTTL() time.Duration {
	return time.Duration(c.SoundCloudClientIDTTLSecond) * time.Second
}
