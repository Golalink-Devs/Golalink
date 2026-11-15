package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golalink/internal/config"
	"golalink/internal/process"
	"golalink/internal/protocol"
)

type Source interface {
	Open(context.Context, *protocol.Track) (io.ReadCloser, error)
}
type Frames interface {
	Next() ([]byte, error)
	Close() error
}
type Engine interface {
	// The context remains live for the whole playback. Canceling it must
	// interrupt both startup and any blocked call to Frames.Next.
	Start(context.Context, *protocol.Track, int64, int, protocol.Object) (Frames, error)
}
type FFmpeg struct {
	Config config.Config
	Source Source
	// Logger receives the decoder lifecycle: process start and exit.
	// Nil falls back to the default logger.
	Logger *slog.Logger
}

// log returns the configured logger so callers never have to check for nil.
func (e *FFmpeg) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

const StuckThreshold = 10 * time.Second

var ErrStuck = errors.New("audio engine did not start or produce a complete frame within 10 seconds")

// ErrFFmpegMissing is returned for a track that needs FFmpeg when none is
// installed. The in-process pipeline takes the common formats, so a node without
// FFmpeg plays normally and only the formats it does not take report this.
var ErrFFmpegMissing = errors.New("this track needs FFmpeg, which is not installed")

func (e *FFmpeg) Start(ctx context.Context, t *protocol.Track, position int64, volume int, filters protocol.Object) (Frames, error) {
	if e.Config.FFmpeg == "" {
		return nil, ErrFFmpegMissing
	}
	body, err := e.Source.Open(ctx, t)
	if err != nil {
		return nil, err
	}
	return e.StartBody(ctx, t, body, position, volume, filters)
}

// StartBody plays a track from an already-open body, which lets a dispatcher
// inspect the leading bytes before choosing an engine. The engine takes
// ownership of the body and closes it with the stream.
func (e *FFmpeg) StartBody(ctx context.Context, t *protocol.Track, body io.ReadCloser, position int64, volume int, filters protocol.Object) (Frames, error) {
	if e.Config.FFmpeg == "" {
		body.Close()
		return nil, ErrFFmpegMissing
	}
	graph, _, err := Build(filters, volume)
	if err != nil {
		body.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "pipe", "-probesize", "1048576", "-analyzeduration", "3000000", "-i", "pipe:0"}
	if position > 0 {
		args = append(args, "-ss", strconv.FormatFloat(float64(position)/1000, 'f', 3, 64))
	}
	args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn", "-af", graph, "-ac", "2", "-ar", "48000", "-c:a", "libopus", "-b:a", strconv.Itoa(e.Config.OpusBitrate), "-application", "audio", "-frame_duration", "20", "-f", "opus", "-page_duration", "20000", "-flush_packets", "1", "pipe:1")
	cmd := exec.CommandContext(ctx, e.Config.FFmpeg, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = body
	stderr := &process.Buffer{Limit: 32 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		body.Close()
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		e.log().Warn("ffmpeg failed to start",
			"identifier", t.Info.Identifier,
			"source", t.Info.SourceName,
			"positionMs", position,
			"volume", volume,
			"error", err,
		)
		stdout.Close()
		body.Close()
		cancel()
		return nil, err
	}
	e.log().Debug("ffmpeg started",
		"pid", cmd.Process.Pid,
		"identifier", t.Info.Identifier,
		"source", t.Info.SourceName,
		"positionMs", position,
		"volume", volume,
		"opusBitrate", e.Config.OpusBitrate,
	)
	return &stream{cmd: cmd, reader: NewOggReader(stdout), body: body, stdout: stdout, cancel: cancel, stderr: stderr,
		logger: e.log(), identifier: t.Info.Identifier, started: time.Now()}, nil
}

// pid reports the decoder process id, or 0 once it is gone.
func (s *stream) pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// logExit records how the decoder ended. The caller has already waited for it,
// so this only reports the outcome.
//
// Stopping playback closes the output pipe, which the encoder reports as a
// muxing error ("Error submitting a packet to the muxer"); that is the normal
// shape of a stop, pause, skip or destroy, so it stays at debug. A decoder that
// ends on its own with a non-zero status is the case worth warning about.
func (s *stream) logExit(reason string) {
	attrs := []any{
		"pid", s.pid(),
		"identifier", s.identifier,
		"reason", reason,
		"playedMs", time.Since(s.started).Milliseconds(),
	}
	switch {
	case s.err == nil:
		s.logger.Debug("ffmpeg exited", attrs...)
	case reason == "closed":
		s.logger.Debug("ffmpeg stopped",
			append(attrs, "error", s.err, "stderr", strings.TrimSpace(s.stderr.String()))...)
	default:
		s.logger.Warn("ffmpeg exited with an error",
			append(attrs, "error", s.err, "stderr", strings.TrimSpace(s.stderr.String()))...)
	}
}

type stream struct {
	cmd        *exec.Cmd
	reader     *OggReader
	body       io.ReadCloser
	stdout     io.ReadCloser
	cancel     context.CancelFunc
	stderr     *process.Buffer
	once       sync.Once
	err        error
	logger     *slog.Logger
	identifier string
	started    time.Time
}

func (s *stream) Next() ([]byte, error) {
	b, e := s.reader.Next()
	if e == io.EOF {
		s.once.Do(func() {
			s.body.Close()
			s.err = s.cmd.Wait()
			s.cancel()
			s.logExit("eof")
		})
		if s.err != nil {
			return nil, fmt.Errorf("audio decoder failed: %w: %.500s", s.err, s.stderr.String())
		}
	}
	return b, e
}
func (s *stream) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.body.Close()
		s.stdout.Close()
		s.err = s.cmd.Wait()
		s.logExit("closed")
	})
	return s.err
}
