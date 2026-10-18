package pure

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"math"

	"github.com/tphakala/go-opus/opus"

	"golalink/internal/audio"
	"golalink/internal/config"
	"golalink/internal/protocol"
)

// frameSamples is one Opus frame at 20 ms and 48 kHz, which is the frame size
// Discord carries. It matches what the FFmpeg engine asks libopus for.
const frameSamples = 960

const frameValues = frameSamples * Channels

// maxPacket is a comfortable ceiling for one Opus packet. The format's own
// maximum for a single frame is well under this.
const maxPacket = 4096

// Engine decodes, resamples and encodes in process. It handles the formats
// Detect and Open cover, and the volume control; anything else belongs to the
// FFmpeg engine.
type Engine struct {
	Config config.Config
	Logger *slog.Logger
}

func (e *Engine) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// SupportsFilters reports whether the pipeline can honour a filter set on its
// own. It parses the request exactly as the FFmpeg engine does and then asks the
// chain whether it can apply every setting, so it can never claim to support a
// filter it would silently drop.
func SupportsFilters(filters protocol.Object, volume int) bool {
	set, err := audio.Parse(filters, volume)
	if err != nil {
		return false
	}
	_, err = newChain(set)
	return err == nil
}

// seekerOf reports the body as a seeker when it is one. A body that cannot seek
// still plays every container whose header comes first.
func seekerOf(body io.ReadCloser) io.ReadSeeker {
	if s, ok := body.(io.ReadSeeker); ok {
		return s
	}
	return nil
}

// StartBody plays a track from an already-open body. The engine takes ownership
// of the body and closes it with the stream.
func (e *Engine) StartBody(ctx context.Context, t *protocol.Track, body io.ReadCloser, position int64, volume int, filters protocol.Object) (audio.Frames, error) {
	set, err := audio.Parse(filters, volume)
	if err != nil {
		body.Close()
		return nil, err
	}
	chain, err := newChain(set)
	if err != nil {
		body.Close()
		return nil, err
	}
	gain := set.Gain
	// The container is read from the body itself, so the dispatcher's decision to
	// route here and this open see the same bytes. Peeking leaves the bytes in
	// the buffer, so a decline below still reads the stream from its first byte.
	br := bufio.NewReaderSize(body, 64<<10)
	head, err := br.Peek(PeekSize)
	if err != nil && len(head) == 0 {
		body.Close()
		return nil, err
	}
	// A track that needs no change is forwarded as it is, which costs nothing
	// and loses nothing. A chain of any kind means the samples have to be
	// touched, so only a bare gain of one qualifies.
	if chain == nil && gain == 1 && webmOpus(head) {
		p, err := newPassthrough(br, body, position)
		if err == nil {
			e.log().Debug("forwarding the stream unchanged",
				"identifier", t.Info.Identifier,
				"source", t.Info.SourceName,
				"positionMs", position,
			)
			_ = ctx
			return p, nil
		}
		// The header is consumed now, so there is no going back here. The
		// dispatcher reopens the source and retries with FFmpeg on an error.
		body.Close()
		return nil, err
	}
	dec, err := open(head, br, seekerOf(body))
	if err != nil {
		body.Close()
		return nil, err
	}
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: OutRate, Channels: Channels, Bitrate: e.Config.OpusBitrate})
	if err != nil {
		dec.Close()
		body.Close()
		return nil, err
	}
	rs := NewResampler(dec.Rate())
	s := &Stream{
		dec:     dec,
		rs:      rs,
		enc:     enc,
		body:    body,
		gain:    float32(gain),
		chain:   chain,
		packet:  make([]byte, maxPacket),
		raw:     make([]byte, 32<<10),
		priming: dec.Skip(),
	}
	// A seek is a decode and discard, which is what FFmpeg's output-side -ss
	// does on a pipe too. The filter delay is part of the offset, so the skip
	// lands on the sample the caller asked for.
	if position > 0 {
		s.skip = int(position)*OutRate/1000 - rs.Latency()
		if s.skip < 0 {
			s.skip = 0
		}
	}
	e.log().Debug("pure pipeline started",
		"identifier", t.Info.Identifier,
		"source", t.Info.SourceName,
		"containerRate", dec.Rate(),
		"channels", dec.Channels(),
		"positionMs", position,
		"volume", volume,
		"opusBitrate", e.Config.OpusBitrate,
	)
	_ = ctx
	return s, nil
}

// Stream yields one Opus packet per Next call.
type Stream struct {
	dec    *Decoder
	rs     *Resampler
	enc    *opus.Encoder
	body   io.ReadCloser
	gain   float32
	chain  *Chain
	packet []byte
	raw    []byte

	stereo    []int16
	resampled []int16
	carry     []int16
	queue     [][]byte

	skip    int
	priming int
	err     error
	closed  bool
}

func (s *Stream) Next() ([]byte, error) {
	for {
		if len(s.queue) > 0 {
			p := s.queue[0]
			s.queue = s.queue[1:]
			return p, nil
		}
		if s.err != nil {
			return nil, s.err
		}
		s.pump()
	}
}

// pump reads one block from the decoder and turns it into as many packets as it
// completes.
func (s *Stream) pump() {
	n, err := s.dec.Read(s.raw)
	if n > 0 {
		src := asInt16(s.raw[:n])
		// Priming is dropped before resampling so the track starts on the sample
		// the container says it does.
		if s.priming > 0 {
			drop := min(s.priming, len(src)/s.dec.Channels())
			src = src[drop*s.dec.Channels():]
			s.priming -= drop
		}
		s.stereo = toStereo(src, s.dec.Channels(), s.stereo[:0])
		s.resampled = s.rs.Process(s.stereo, s.resampled[:0])
		if s.skip > 0 {
			frames := len(s.resampled) / Channels
			if frames > s.skip {
				frames = s.skip
			}
			s.resampled = s.resampled[frames*Channels:]
			s.skip -= frames
		}
		if s.gain != 1 {
			for i, v := range s.resampled {
				s.resampled[i] = scale(v, s.gain)
			}
		}
		// The graph runs the filters after the volume control, so the chain sees
		// the gained samples.
		if s.chain != nil {
			s.resampled = s.chain.process(s.resampled)
		}
		if len(s.resampled) > 0 {
			s.carry = append(s.carry, s.resampled...)
			s.encode()
		}
	}
	if err == nil {
		return
	}
	if err != io.EOF {
		s.err = err
		s.finish()
		return
	}
	// Encode the last partial frame padded with silence, so nothing is dropped.
	// A chain that buffers has to be asked for its tail first.
	if s.chain != nil {
		s.carry = append(s.carry, s.chain.flush()...)
	}
	if len(s.carry) > 0 {
		for len(s.carry) < frameValues {
			s.carry = append(s.carry, 0)
		}
		s.encode()
	}
	s.err = io.EOF
	s.finish()
}

// encode turns every complete frame in carry into a packet.
func (s *Stream) encode() {
	for len(s.carry) >= frameValues {
		n, err := s.enc.Encode(s.carry[:frameValues], s.packet)
		if err != nil {
			s.err = err
			return
		}
		if n > 0 {
			out := make([]byte, n)
			copy(out, s.packet[:n])
			s.queue = append(s.queue, out)
		}
		s.carry = append(s.carry[:0], s.carry[frameValues:]...)
	}
}

// finish releases the decoder once the stream ends.
func (s *Stream) finish() {
	if s.body != nil {
		s.body.Close()
		s.body = nil
	}
	s.dec.Close()
}

func (s *Stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.finish()
	return nil
}

// scale applies gain and saturates. A request may ask for more than unity, where
// clipping is the correct answer rather than wrapping.
func scale(v int16, g float32) int16 {
	f := float64(v) * float64(g)
	if f >= 32767 {
		return 32767
	}
	if f <= -32768 {
		return -32768
	}
	return int16(f + math.Copysign(0.5, f))
}
