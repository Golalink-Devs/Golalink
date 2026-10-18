// Package dispatch routes a track to the engine that can play it: the in-process
// pipeline for the containers it covers, and FFmpeg for everything else.
package dispatch

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"golalink/internal/audio"
	"golalink/internal/audio/pure"
	"golalink/internal/protocol"
)

// BodyEngine plays from a body the caller has already opened, so a dispatcher
// can look at the leading bytes before choosing one.
type BodyEngine interface {
	StartBody(ctx context.Context, t *protocol.Track, body io.ReadCloser, position int64, volume int, filters protocol.Object) (audio.Frames, error)
}

// Dispatcher implements audio.Engine by picking between two engines.
type Dispatcher struct {
	Source audio.Source
	// Pure is the in-process pipeline. A nil Pure sends everything to Fallback.
	Pure BodyEngine
	// Fallback is FFmpeg, which every format and every filter graph works with.
	Fallback BodyEngine
	Logger   *slog.Logger
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// peeked is a body whose leading bytes have been read into a buffer but not
// consumed, so whichever engine ends up with it sees the whole stream.
type peeked struct {
	*bufio.Reader
	body io.ReadCloser
}

func (p *peeked) Close() error { return p.body.Close() }

// Seek passes a seek through to the source, dropping the buffer first: every
// byte in it came from before the new position. A container that keeps its
// header at the end needs this, and one that does not never asks for it.
func (p *peeked) Seek(offset int64, whence int) (int64, error) {
	pos, err := seekBody(p.body, offset, whence)
	if err != nil {
		return 0, err
	}
	p.Reader.Reset(p.body)
	return pos, nil
}

// seekBody seeks a body that can seek and names the reason when it cannot.
func seekBody(body io.ReadCloser, offset int64, whence int) (int64, error) {
	s, ok := body.(io.Seeker)
	if !ok {
		return 0, errors.New("dispatch: the source cannot seek")
	}
	return s.Seek(offset, whence)
}

// onceBody makes closing idempotent. Whichever engine takes the body owns it and
// closes it, and the retry below closes it too when the in-process pipeline
// declines it, so the same body can be closed on both paths.
type onceBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (o *onceBody) Close() error {
	o.once.Do(func() { o.err = o.ReadCloser.Close() })
	return o.err
}

// Seek reports the underlying body's ability to seek rather than always claiming
// one, so a source that cannot seek is not mistaken for one that can.
func (o *onceBody) Seek(offset int64, whence int) (int64, error) {
	return seekBody(o.ReadCloser, offset, whence)
}

func (d *Dispatcher) Start(ctx context.Context, t *protocol.Track, position int64, volume int, filters protocol.Object) (audio.Frames, error) {
	opened, err := d.Source.Open(ctx, t)
	if err != nil {
		return nil, err
	}
	body := &onceBody{ReadCloser: opened}
	if d.Pure == nil {
		return d.Fallback.StartBody(ctx, t, body, position, volume, filters)
	}
	// The filter graph decides as much as the container does: ask the in-process
	// pipeline whether it can apply this exact request before reading anything.
	if !pure.SupportsFilters(filters, volume) {
		return d.Fallback.StartBody(ctx, t, body, position, volume, filters)
	}
	buffered := bufio.NewReaderSize(body, 64<<10)
	head, _ := buffered.Peek(pure.PeekSize)
	if !pure.Detect(head) {
		d.log().Debug("using ffmpeg",
			"identifier", t.Info.Identifier,
			"source", t.Info.SourceName,
			"reason", "container",
		)
		return d.Fallback.StartBody(ctx, t, &peeked{Reader: buffered, body: body}, position, volume, filters)
	}
	frames, err := d.Pure.StartBody(ctx, t, &peeked{Reader: buffered, body: body}, position, volume, filters)
	if err == nil {
		d.log().Debug("using the in-process pipeline",
			"identifier", t.Info.Identifier,
			"source", t.Info.SourceName,
			"positionMs", position,
		)
		return frames, nil
	}
	// The in-process pipeline reads the body before it can reject a stream, so
	// the fall back starts from a fresh one rather than a half-read stream.
	d.log().Warn("in-process pipeline declined the track; retrying with ffmpeg",
		"identifier", t.Info.Identifier,
		"source", t.Info.SourceName,
		"error", err,
	)
	body.Close()
	fresh, ferr := d.Source.Open(ctx, t)
	if ferr != nil {
		return nil, ferr
	}
	return d.Fallback.StartBody(ctx, t, &onceBody{ReadCloser: fresh}, position, volume, filters)
}

// compile-time checks that both engines satisfy the contract the dispatcher
// expects.
var (
	_ BodyEngine = (*audio.FFmpeg)(nil)
	_ BodyEngine = (*pure.Engine)(nil)
)
