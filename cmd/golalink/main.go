package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golalink/internal/audio"
	"golalink/internal/audio/dispatch"
	"golalink/internal/audio/pure"
	"golalink/internal/config"
	"golalink/internal/logging"
	"golalink/internal/lyrics"
	"golalink/internal/server"
	"golalink/internal/source"
	"golalink/internal/tools"
	"golalink/internal/voice"
	"golalink/internal/webhook"
)

// helperTimeout bounds the download of a missing media helper. A first start on
// an empty machine moves a few hundred MiB, so the limit is generous.
const helperTimeout = 20 * time.Minute

// isNone reports a helper the configuration has switched off. Naming none is how
// a setting says it wants no such helper, which is a different thing from naming
// one that cannot be found, and the two are reported differently.
func isNone(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "none")
}

func main() {
	if err := run(); err != nil {
		slog.Error("Golalink stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "", "YAML configuration file (default: config.yaml, config.yml or config.json)")
	version := flag.Bool("version", false, "print Golalink version")
	check := flag.Bool("check", false, "validate configuration and media helpers, then exit")
	flag.Parse()
	if *version {
		fmt.Println("Golalink " + server.Version + " (Lavalink API " + server.APIVersion + ")")
		return nil
	}
	configPath := config.Discover(*path)
	c, err := config.Load(configPath)
	if err != nil {
		return err
	}
	format := strings.ToLower(strings.TrimSpace(c.LogFormat))
	// A Windows console needs two settings before it can show colour escapes and
	// non-ASCII text; everywhere else this does nothing.
	logging.PrepareConsole(os.Stdout)
	// The wordmark is for a person watching a terminal, so it stays out of a
	// JSON stream that something else is parsing.
	if !*check && format != "json" {
		color := logging.UseColor(strings.ToLower(strings.TrimSpace(c.LogColor)), os.Stdout)
		logging.PrintBanner(os.Stdout, color, logging.UnicodeConsole(os.Stdout), server.Version, server.APIVersion)
	}
	logger, logFile, logPath, err := logging.New(logging.Options{
		Level:     logging.ParseLevel(strings.ToLower(strings.TrimSpace(c.LogLevel))),
		Format:    format,
		Color:     strings.ToLower(strings.TrimSpace(c.LogColor)),
		Directory: strings.TrimSpace(c.LogDirectory),
		Console:   os.Stdout,
	})
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	if logFile != nil {
		defer logFile.Close()
		logger.Info("logging to file", "path", logPath, "directory", c.LogDirectory)
	}
	// Media helpers are resolved before anything else starts: a copy in the tools
	// directory is used as it is, and a missing one is downloaded into it, so a
	// fresh checkout needs no preparation step and never picks up whatever build
	// of FFmpeg the machine happens to have on PATH.
	resolver := &tools.Resolver{Directory: c.ToolsDirectory, Download: c.AutoDownload, Logger: logger}
	defer resolver.Close()
	helpers, stopHelpers := context.WithTimeout(context.Background(), helperTimeout)
	defer stopHelpers()
	// FFmpeg is the fall back for the few formats the in-process pipeline does
	// not take, so a node without it still plays everything else. Losing it
	// costs those formats rather than the node, which is why this is a warning
	// and not the end of the start up.
	if isNone(c.FFmpeg) {
		// Not wanted at all. That is a choice rather than a failure, so it is
		// reported as one and nothing is downloaded for it.
		c.FFmpeg = ""
		logger.Info("ffmpeg disabled; tracks in the formats the in-process pipeline does not take will fail to load")
	} else if ffmpeg, err := resolver.Resolve(helpers, "ffmpeg", c.FFmpeg); err != nil {
		c.FFmpeg = ""
		logger.Warn("ffmpeg unavailable; tracks in the formats the in-process pipeline does not take will fail to load",
			"error", err)
	} else {
		c.FFmpeg = ffmpeg
		logger.Debug("media helper ready", "ffmpeg", ffmpeg)
	}
	// Native clients cover YouTube and SoundCloud on their own, so a missing
	// yt-dlp only removes the fallback and the generic extractors.
	if !c.YTDLPEnabled {
		logger.Info("yt-dlp fallback disabled; native sources only")
	} else if resolved, err := resolver.Resolve(helpers, "yt-dlp", c.YTDLP); err != nil {
		c.YTDLPEnabled = false
		logger.Warn("yt-dlp unavailable; native YouTube and SoundCloud still work, but the fallback and generic extractors are unavailable", "error", err)
	} else {
		c.YTDLP = resolved
		logger.Info("yt-dlp fallback available", "path", resolved)
	}
	if *check {
		// Resolving the helpers above can leave the configuration in a state the
		// server would refuse, so it is validated again here rather than reported
		// as valid on the strength of the check that ran when it was loaded.
		if err := c.Validate(); err != nil {
			return fmt.Errorf("configuration is not valid: %w", err)
		}
		logger.Info("configuration valid",
			"config", configPath,
			"sources", sourceSummary(c),
			"logLevel", c.LogLevel,
			"logFormat", c.LogFormat,
			"logColor", c.LogColor,
			"logDirectory", c.LogDirectory,
		)
		return nil
	}
	logger.Info("Golalink starting",
		"version", server.Version,
		"apiVersion", server.APIVersion,
		"config", configPath,
		"sources", sourceSummary(c),
		"toolsDirectory", c.ToolsDirectory,
		"autoDownload", c.AutoDownload,
		"maxPlayers", c.MaxPlayers,
		"maxSessions", c.MaxSessions,
		"maxConcurrentLoads", c.MaxConcurrentLoads,
		"cacheEntries", c.CacheEntries,
		"cacheTtlSeconds", c.CacheTTLSeconds,
		"loadTimeoutSeconds", c.LoadTimeoutSeconds,
		"eventBuffer", c.EventBuffer,
		"opusBitrate", c.OpusBitrate,
		"allowPrivateNetworks", c.AllowPrivateNetworks,
		"ratelimits", c.RateLimits.Enable,
		"webhook", c.Webhook.Enable,
	)
	sources := source.New(c)
	sources.Logger = logger
	defer sources.Close()
	ffmpegEngine := &audio.FFmpeg{Config: c, Source: sources, Logger: logger}
	engine := audio.Engine(ffmpegEngine)
	if c.InProcessAudio {
		engine = &dispatch.Dispatcher{
			Source:   sources,
			Pure:     &pure.Engine{Config: c, Logger: logger},
			Fallback: ffmpegEngine,
			Logger:   logger,
		}
	}
	// Lyrics come from a public service, so the node only calls one when the
	// configuration names it. The database answers most tracks and the catalogue
	// answers the ones it has nothing for, which is where timed lines come from.
	var lyricManager *lyrics.Manager
	if c.LyricsSource == "lrclib" {
		catalogue := lyrics.NewYTMusic(sources.HTTP)
		catalogue.Endpoint = c.YoutubeMusicEndpoint
		catalogue.Version = c.YoutubeClientVersion
		lyricManager = lyrics.New(lyrics.NewLRCLIB(sources.HTTP), catalogue)
	}
	// A configured webhook gets a worker before the server starts, so it is
	// ready for the first event; a disabled one yields a nil client that
	// ignores every event.
	webhooks := webhook.New(c.Webhook, logger)
	srv, err := server.New(c, server.Dependencies{
		Loader:  sources,
		Engine:  engine,
		Voice:   voice.Discord{Logger: logger},
		Logger:  logger,
		Lyrics:  lyricManager,
		Webhook: webhooks,
	})
	if err != nil {
		return err
	}
	defer srv.Close()
	httpServer := &http.Server{Addr: c.Address, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("Golalink listening", "address", c.Address, "version", server.Version)
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case err = <-errCh:
		if err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}
	srv.Close()
	sources.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = httpServer.Shutdown(shutdown)
	logger.Info("Golalink stopped")
	return err
}

// sourceSummary describes which source paths are active, for the startup log.
func sourceSummary(c config.Config) string {
	var on []string
	if c.YoutubeNative {
		on = append(on, "youtube(native)")
	}
	if c.SoundCloudNative {
		on = append(on, "soundcloud(native)")
	}
	if c.YTDLPEnabled {
		on = append(on, "yt-dlp(fallback)")
	}
	if c.SpotifyClientID != "" {
		on = append(on, "spotify")
	}
	if c.SpotifyRefreshToken != "" {
		on = append(on, "spotify(user-token)")
	}
	on = append(on, "deezer", "http")
	return strings.Join(on, ",")
}
