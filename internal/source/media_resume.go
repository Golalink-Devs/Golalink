package source

import (
	"context"
	"errors"
	"io"
	"time"
)

// mediaResumeAttempts bounds how often one playback may continue a media stream
// that the source dropped. mediaResumeDelay is the pause before the first
// attempt; later attempts wait proportionally longer.
const (
	mediaResumeAttempts = 3
	mediaResumeDelay    = 250 * time.Millisecond
)

// resumableBody keeps a media stream flowing when the connection behind it is
// dropped mid-track.
//
// The decoder reads the media through this process, so a dropped connection
// surfaces as a read error that would otherwise end the track. Both CDNs this
// node streams from answer byte ranges, so the read is continued by asking for
// the byte after the last one delivered, and the decoder never notices. Field
// evidence (2026-10-08): playback ran for 147 seconds of a Spotify-mirrored
// track before the CDN reset the connection
// ("wsarecv: An existing connection was forcibly closed by the remote host"),
// which ended the track with reason=loadFailed.
//
// The bytes already read are always handed to the caller before a continuation
// is arranged, so a failure at the very start of a track still fails fast.
type resumableBody struct {
	ctx      context.Context
	svc      *Service
	raw      string
	headers  map[string]string
	body     io.ReadCloser
	offset   int64
	attempts int
	pending  error
	closed   bool
	// size is the whole length of the media, or -1 when the source did not say.
	// Seeking from the end needs it.
	size int64
}

// newResumableBody wraps a playback body. The context must be the one that lives
// for the whole playback, because a continuation is requested on it.
func (s *Service) newResumableBody(ctx context.Context, raw string, headers map[string]string, body io.ReadCloser, size int64) io.ReadCloser {
	return &resumableBody{ctx: ctx, svc: s, raw: raw, headers: headers, body: body, size: size}
}

// Seek gives the body random access, which a container that keeps its header
// after its media needs in order to find it.
//
// The source answers byte ranges, so a seek is a new request from the wanted
// offset rather than a jump within the open connection. A track that never seeks
// reads straight through and pays nothing for this.
func (r *resumableBody) Seek(offset int64, whence int) (int64, error) {
	target, err := r.target(offset, whence)
	if err != nil {
		return 0, err
	}
	if target == r.offset {
		return target, nil
	}
	// A position at or past the end is not a range the source can serve: a
	// request that starts at the end of the file is refused outright. Reads from
	// there end the stream instead.
	if r.size >= 0 && target >= r.size {
		r.body.Close()
		r.body = emptyBody{}
		r.offset = target
		r.pending = nil
		return target, nil
	}
	if _, err := r.reopen(target); err != nil {
		return 0, err
	}
	return target, nil
}

// emptyBody is what a body reads from once it has been seeked to its end.
type emptyBody struct{}

func (emptyBody) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyBody) Close() error             { return nil }

// target resolves a seek against the position the body has reached.
func (r *resumableBody) target(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.offset + offset
	case io.SeekEnd:
		if r.size < 0 {
			return 0, errors.New("source did not report the length of the media")
		}
		target = r.size + offset
	default:
		return 0, errors.New("invalid seek origin")
	}
	if target < 0 {
		return 0, errors.New("seek before the start of the media")
	}
	return target, nil
}

// reopen points the body at a new offset and reports whether the source honoured
// the range. A source that ignores it restarts at zero, so the prefix is
// discarded to leave the byte stream starting where the caller asked.
func (r *resumableBody) reopen(target int64) (bool, error) {
	body, honoured, err := r.svc.fetchStreamRange(r.ctx, r.raw, r.headers, target)
	if err != nil {
		return false, err
	}
	if !honoured && target > 0 {
		if _, derr := io.CopyN(io.Discard, body, target); derr != nil {
			body.Close()
			return honoured, derr
		}
	}
	r.body.Close()
	r.body = body
	r.offset = target
	r.pending = nil
	return honoured, nil
}

func (r *resumableBody) Read(p []byte) (int, error) {
	if r.pending != nil {
		if err := r.resume(); err != nil {
			return 0, err
		}
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if n > 0 {
		// Hand over what arrived and arrange the continuation on the next call.
		r.pending = err
		return n, nil
	}
	// The error is recorded even when no byte arrived, because resume reports it
	// when every attempt fails; leaving it unset here would loop forever.
	r.pending = err
	if rerr := r.resume(); rerr != nil {
		return 0, rerr
	}
	return r.Read(p)
}

// resume requests the rest of the media, starting at the current offset. Another
// attempt is made only while attempts remain; the original read error is what a
// caller ultimately sees, because it is the cause an operator needs.
func (r *resumableBody) resume() error {
	last := r.pending
	r.pending = nil
	for r.attempts < mediaResumeAttempts {
		if err := r.ctx.Err(); err != nil {
			return last
		}
		r.attempts++
		delay := time.Duration(r.attempts) * mediaResumeDelay
		select {
		case <-r.ctx.Done():
			return last
		case <-time.After(delay):
		}
		honoured, err := r.reopen(r.offset)
		if err != nil {
			r.svc.log().Warn("media stream resume attempt failed",
				"offset", r.offset,
				"attempt", r.attempts,
				"error", err,
			)
			continue
		}
		r.svc.log().Info("media stream resumed",
			"offset", r.offset,
			"attempt", r.attempts,
			"rangeHonoured", honoured,
		)
		return nil
	}
	r.svc.log().Warn("media stream could not be resumed",
		"offset", r.offset,
		"attempts", r.attempts,
		"error", last,
	)
	return last
}

func (r *resumableBody) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.body.Close()
}
