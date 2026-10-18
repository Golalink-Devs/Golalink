package pure

import (
	"encoding/binary"
	"io"
)

// samples converts a decoder's packed samples into the 16-bit samples the rest
// of the pipeline works in.
//
// FLAC keeps whatever depth the file was mastered at, and reading a wider sample
// as if it were 16 bits would turn it into noise rather than merely losing
// precision, so the conversion happens here instead. FFmpeg carries the extra
// bits all the way to the encoder; this quantises to 16, which is a difference
// the encoder could not have used at the bitrates Golalink sends.
type samples struct {
	r       io.Reader
	width   int // bytes per sample in the source
	shift   int // bits to move right to reach 16, or left when negative
	read    []byte
	pending []byte
	out     []byte
	off     int
	err     error
}

func newSamples(r io.Reader, bitDepth int) *samples {
	return &samples{
		r:       r,
		width:   (bitDepth + 7) / 8,
		shift:   bitDepth - 16,
		read:    make([]byte, 16<<10),
		pending: make([]byte, 0, (16<<10)+4),
	}
}

func (s *samples) Read(p []byte) (int, error) {
	for s.off >= len(s.out) {
		if s.err != nil {
			return 0, s.err
		}
		if err := s.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.out[s.off:])
	s.off += n
	return n, nil
}

func (s *samples) fill() error {
	n, err := s.r.Read(s.read)
	if n == 0 && err == nil {
		return io.ErrNoProgress
	}
	if n > 0 {
		s.pending = append(s.pending, s.read[:n]...)
		whole := len(s.pending) / s.width
		s.out = s.out[:0]
		for i := range whole {
			v := packed(s.pending[i*s.width:], s.width)
			s.out = binary.LittleEndian.AppendUint16(s.out, uint16(s.convert(v)))
		}
		// A sample split across two reads stays behind for the next one.
		s.pending = append(s.pending[:0], s.pending[whole*s.width:]...)
		s.off = 0
	}
	if err != nil {
		// Whatever was read before the error is still returned, and the error
		// waits for the call after it.
		if s.off < len(s.out) {
			s.err = err
			return nil
		}
		return err
	}
	return nil
}

// convert scales one sample to 16 bits, rounding rather than truncating so a
// quiet passage does not drift towards zero. A source wider than 16 bits has to
// come down and a narrower one has to go up, which is what the sign of shift
// says.
func (s *samples) convert(v int32) int16 {
	switch {
	case s.shift == 0:
		return int16(v)
	case s.shift > 0:
		return clamp16(float64(v) / float64(int32(1)<<s.shift))
	default:
		return clamp16(float64(v) * float64(int32(1)<<-s.shift))
	}
}

// packed reads one little-endian signed sample of the given width. FLAC stores
// the sample value itself, not shifted up to fill the bytes, so a 24-bit sample
// arrives as a signed 24-bit number in three bytes.
func packed(b []byte, width int) int32 {
	switch width {
	case 1:
		return int32(int8(b[0]))
	case 2:
		return int32(int16(binary.LittleEndian.Uint16(b)))
	case 3:
		v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
		if v&(1<<23) != 0 {
			v -= 1 << 24
		}
		return v
	default:
		return int32(binary.LittleEndian.Uint32(b))
	}
}
