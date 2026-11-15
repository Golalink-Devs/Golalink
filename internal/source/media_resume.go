package source

import (
	"context"
	"errors"
	"io"
	"sync"
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
// Bytes already read are handed to the caller before a continuation is made.
// A complete partial response is followed immediately by the next byte range.
type resumableBody struct {
	ctx      context.Context
	cancel   context.CancelFunc
	svc      *Service
	raw      string
	headers  map[string]string
	offset   int64
	attempts int
	pending  error

	// Only one goroutine reads or seeks, but FFmpeg can close the body while
	// its stdin is reading or opening a new response.
	mu     sync.Mutex
	media  mediaResponse
	closed bool
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
	if size := r.media.size; size >= 0 && target >= size {
		if err := r.replace(mediaResponse{body: emptyBody{}, size: size, end: target}, target); err != nil {
			return 0, err
		}
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
		if r.media.size < 0 {
			return 0, errors.New("source did not report the length of the media")
		}
		target = r.media.size + offset
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
	media, err := r.svc.fetchMedia(r.ctx, r.raw, r.headers, true, target)
	if err != nil {
		return false, err
	}
	if size := r.media.size; size >= 0 {
		if media.size >= 0 && media.size != size || media.end > size {
			media.body.Close()
			return media.partial, errors.New("media length changed between byte ranges")
		}
		media.size = size
		if media.end < 0 {
			media.end = size
		}
	}
	if !media.partial && target > 0 {
		if _, err := io.CopyN(io.Discard, media.body, target); err != nil {
			media.body.Close()
			return false, err
		}
	}
	return media.partial, r.replace(media, target)
}

func (r *resumableBody) replace(media mediaResponse, target int64) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		media.body.Close()
		return io.ErrClosedPipe
	}
	old := r.media.body
	r.media = media
	r.mu.Unlock()
	old.Close()
	r.offset = target
	r.pending = nil
	return nil
}

func (r *resumableBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.pending != nil {
			if err := r.resume(); err != nil {
				return 0, err
			}
		}
		if r.media.end >= 0 && r.offset == r.media.end {
			if !r.media.partial || r.offset == r.media.size {
				return 0, io.EOF
			}
			// The server sent a complete segment, not a broken connection.
			// Request the next segment without spending the retry budget.
			if _, err := r.reopen(r.offset); err != nil {
				return 0, err
			}
			continue
		}
		buf := p
		if remaining := r.media.end - r.offset; r.media.end >= 0 && remaining < int64(len(buf)) {
			buf = buf[:remaining]
		}
		n, err := r.media.body.Read(buf)
		r.offset += int64(n)
		if err == nil || r.offset == r.media.end {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			if r.media.end < 0 {
				return n, io.EOF
			}
			err = io.ErrUnexpectedEOF
		}
		r.pending = err
		if n > 0 {
			// Hand over what arrived and arrange the recovery on the next call.
			return n, nil
		}
	}
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
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	body := r.media.body
	r.mu.Unlock()
	r.cancel()
	return body.Close()
}
