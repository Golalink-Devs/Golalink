package pure

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/jfreymuth/oggvorbis"
)

// looksVorbis reports whether an Ogg stream carries Vorbis. Ogg is only a
// container and holds more than one codec, so the identification header at the
// front of the first packet is what tells them apart: Opus spells out OpusHead
// and Vorbis starts with a byte of one followed by its name.
func looksVorbis(head []byte) bool {
	at := bytes.Index(head, []byte("\x01vorbis"))
	return at >= 0 && at < 64
}

// vorbisReader turns the samples a Vorbis decoder hands back into the 16-bit
// samples the rest of the pipeline works in. The decoder works in floats
// normalised to one, which is the same scale the encoder wants, so the only
// question is where to put the fixed point.
type vorbisReader struct {
	v   *oggvorbis.Reader
	pcm []float32
	out []byte
	off int
	err error
}

func newVorbis(v *oggvorbis.Reader) *vorbisReader {
	return &vorbisReader{v: v, pcm: make([]float32, 4096*v.Channels())}
}

func (r *vorbisReader) Read(p []byte) (int, error) {
	for r.off >= len(r.out) {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.out[r.off:])
	r.off += n
	return n, nil
}

func (r *vorbisReader) fill() error {
	n, err := r.v.Read(r.pcm)
	r.out = r.out[:0]
	for _, s := range r.pcm[:n] {
		r.out = binary.LittleEndian.AppendUint16(r.out, uint16(clampNormalized(float64(s))))
	}
	r.off = 0
	if err != nil {
		// Samples read before the error are still returned, and the error waits
		// for the call after them.
		if len(r.out) > 0 {
			r.err = err
			return nil
		}
		return err
	}
	if n == 0 {
		return io.ErrNoProgress
	}
	return nil
}
