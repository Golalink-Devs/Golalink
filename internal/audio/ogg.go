package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// OggReader reassembles laced packets, including packets spanning pages.
// It never buffers more than one page and one bounded packet.
type OggReader struct {
	r                io.Reader
	segments         []byte
	body             []byte
	index, offset    int
	packet           []byte
	serial, sequence uint32
	started          bool
	headers          int
}

func NewOggReader(r io.Reader) *OggReader { return &OggReader{r: r} }
func (o *OggReader) page() error {
	var h [27]byte
	if _, e := io.ReadFull(o.r, h[:]); e != nil {
		return e
	}
	if string(h[:4]) != "OggS" || h[4] != 0 {
		return errors.New("invalid Ogg page")
	}
	serial, seq := binary.LittleEndian.Uint32(h[14:18]), binary.LittleEndian.Uint32(h[18:22])
	if o.started && (serial != o.serial || seq != o.sequence+1) {
		return errors.New("discontinuous Ogg stream")
	}
	if (h[5]&1 != 0) != (len(o.packet) > 0) {
		return errors.New("invalid Ogg continuation")
	}
	o.started = true
	o.serial = serial
	o.sequence = seq
	if cap(o.segments) < int(h[26]) {
		o.segments = make([]byte, int(h[26]))
	} else {
		o.segments = o.segments[:int(h[26])]
	}
	if _, e := io.ReadFull(o.r, o.segments); e != nil {
		return e
	}
	n := 0
	for _, v := range o.segments {
		n += int(v)
	}
	if cap(o.body) < n {
		o.body = make([]byte, n)
	} else {
		o.body = o.body[:n]
	}
	if _, e := io.ReadFull(o.r, o.body); e != nil {
		return e
	}
	checksum := binary.LittleEndian.Uint32(h[22:26])
	clear(h[22:26])
	crc := oggCRC(0, h[:])
	crc = oggCRC(crc, o.segments)
	crc = oggCRC(crc, o.body)
	if checksum != crc {
		return errors.New("Ogg checksum mismatch")
	}
	o.index = 0
	o.offset = 0
	return nil
}

var crcTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(crc uint32, b []byte) uint32 {
	for _, v := range b {
		crc = crc<<8 ^ crcTable[byte(crc>>24)^v]
	}
	return crc
}
func (o *OggReader) Next() ([]byte, error) {
	for {
		if o.index == len(o.segments) {
			if e := o.page(); e != nil {
				if e == io.EOF && len(o.packet) > 0 {
					return nil, io.ErrUnexpectedEOF
				}
				return nil, e
			}
		}
		for o.index < len(o.segments) {
			n := int(o.segments[o.index])
			o.index++
			if len(o.packet)+n > 65536 {
				return nil, errors.New("Ogg packet exceeds limit")
			}
			o.packet = append(o.packet, o.body[o.offset:o.offset+n]...)
			o.offset += n
			if n == 255 {
				continue
			}
			packet := o.packet
			o.packet = o.packet[:0]
			if o.headers == 0 {
				if len(packet) < 19 || !bytes.HasPrefix(packet, []byte("OpusHead")) || packet[9] != 2 {
					return nil, errors.New("expected stereo Opus stream")
				}
				o.headers++
				continue
			}
			if o.headers == 1 {
				if !bytes.HasPrefix(packet, []byte("OpusTags")) {
					return nil, errors.New("missing Opus tags")
				}
				o.headers++
				continue
			}
			if len(packet) == 0 {
				return nil, errors.New("empty Opus packet")
			}
			return packet, nil
		}
	}
}
