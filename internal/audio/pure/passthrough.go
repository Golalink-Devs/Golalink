package pure

import (
	"fmt"
	"io"

	"golalink/internal/protocol"
)

// A stream that needs no change is forwarded rather than re-encoded. Decoding
// Opus and encoding it again costs a full round trip and cannot add anything:
// what Discord receives is then the encoder's second guess at audio it was
// already given. Matroska stores Opus packets whole, so they can be handed on
// untouched, and the bytes Discord plays are the bytes the source served.
//
// This only applies when nothing has to change. A volume other than unity, or
// any filter at all, needs the samples, and those tracks take the decoding path.

// passthrough forwards the Opus packets of a WebM stream unchanged.
type passthrough struct {
	w      *webm
	body   io.ReadCloser
	seekTo int64
	// first holds the packet the constructor read to check the frame size.
	first   []byte
	firstTS int64
	framed  bool
	closed  bool
}

// newPassthrough takes over a stream positioned at its start. It fails if the
// stream is not Opus in WebM, or if its frames are not the 20 ms the player
// paces at, and the caller then decodes instead.
func newPassthrough(r io.Reader, body io.ReadCloser, position int64) (*passthrough, error) {
	w, _, err := openWebM(r)
	if err != nil {
		return nil, err
	}
	// The frame size is fixed for the life of a stream, so the first packet
	// settles whether this one can be forwarded.
	pkt, ts, err := w.next()
	if err != nil {
		return nil, err
	}
	if got := opusPacketSamples(pkt); got != frameSamples {
		return nil, fmt.Errorf("pure: Opus frames of %d samples cannot be forwarded", got)
	}
	return &passthrough{w: w, body: body, seekTo: position, first: pkt, firstTS: ts, framed: true}, nil
}

// Next returns the next packet of the source.
func (p *passthrough) Next() ([]byte, error) {
	if p.first != nil {
		pkt, ts := p.first, p.firstTS
		p.first = nil
		if ts >= p.seekTo {
			return pkt, nil
		}
	}
	for {
		pkt, ts, err := p.w.next()
		if err != nil {
			return nil, err
		}
		// A seek drops whole packets. The container supplies a timestamp for
		// each one, so landing within a frame of the target costs nothing but
		// the frame that contains it.
		if ts < p.seekTo {
			continue
		}
		if got := opusPacketSamples(pkt); got != frameSamples {
			return nil, fmt.Errorf("pure: Opus frame size changed to %d samples", got)
		}
		return pkt, nil
	}
}

func (p *passthrough) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	if p.body != nil {
		return p.body.Close()
	}
	return nil
}

// needsNoProcessing reports whether a request leaves the samples alone, which is
// the only case a stream can be forwarded verbatim.
func needsNoProcessing(filters protocol.Object, gain float64) bool {
	if gain != 1 {
		return false
	}
	for name := range filters {
		switch name {
		case "volume", "pluginFilters":
			// Both fold into the gain checked above.
			continue
		}
		return false
	}
	return true
}
