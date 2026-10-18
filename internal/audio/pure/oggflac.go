package pure

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	flacpcm "github.com/tphakala/go-flac/pcm"
)

// looksOggFlac reports FLAC inside an Ogg stream. The mapping header names the
// codec before the native stream begins, the same way Vorbis and Opus do.
func looksOggFlac(head []byte) bool {
	at := bytes.Index(head, []byte("\x7fFLAC"))
	return at >= 0 && at < 64
}

// oggPages reads the packets of one logical Ogg stream. Ogg is a container of
// pages, and a packet is a run of segments that ends at a segment shorter than
// 255, so a packet can span pages and a page usually holds several packets. The
// pages of another stream can also be interleaved with these.
type oggPages struct {
	r      io.Reader
	serial uint32
	// table and body are the page being consumed, and seg and at are how far
	// through it the reader has got. A page outlives the call that read it,
	// because it usually carries more than one packet.
	table []byte
	body  []byte
	seg   int
	at    int
	part  []byte
}

func newOggPages(r io.Reader) *oggPages {
	return &oggPages{r: r}
}

func (o *oggPages) next() ([]byte, error) {
	for {
		if o.seg < len(o.table) {
			s := int(o.table[o.seg])
			o.seg++
			o.part = append(o.part, o.body[o.at:o.at+s]...)
			o.at += s
			if s < 255 {
				packet := o.part
				o.part = nil
				return packet, nil
			}
			continue
		}
		if err := o.readPage(); err != nil {
			return nil, err
		}
	}
}

// readPage loads the next page that belongs to this stream. Pages from another
// stream are skipped whole, so the reader stays in step with the file.
func (o *oggPages) readPage() error {
	header := make([]byte, 27)
	for {
		if _, err := io.ReadFull(o.r, header); err != nil {
			return err
		}
		if string(header[:4]) != "OggS" {
			return errors.New("pure: not an Ogg page")
		}
		segments := int(header[26])
		if cap(o.table) < segments {
			o.table = make([]byte, segments)
		}
		o.table = o.table[:segments]
		if _, err := io.ReadFull(o.r, o.table); err != nil {
			return err
		}
		total := 0
		for _, s := range o.table {
			total += int(s)
		}
		if cap(o.body) < total {
			o.body = make([]byte, total)
		}
		o.body = o.body[:total]
		if _, err := io.ReadFull(o.r, o.body); err != nil {
			return err
		}
		serial := binary.LittleEndian.Uint32(header[14:18])
		if o.serial == 0 {
			o.serial = serial
		} else if serial != o.serial {
			continue
		}
		o.seg, o.at = 0, 0
		return nil
	}
}

// oggFlacReader presents an Ogg FLAC stream as the native FLAC stream the FLAC
// reader expects: the signature and the metadata blocks first, then the frames.
// The container splits the two across packets and the native reader wants one
// run of bytes, so the seam is stitched here.
type oggFlacReader struct {
	pages *oggPages
	head  []byte
	off   int
	pkt   []byte
	err   error
}

func (o *oggFlacReader) Read(p []byte) (int, error) {
	if o.off < len(o.head) {
		n := copy(p, o.head[o.off:])
		o.off += n
		return n, nil
	}
	for len(o.pkt) == 0 {
		if o.err != nil {
			return 0, o.err
		}
		pkt, err := o.pages.next()
		if err != nil {
			o.err = err
			return 0, err
		}
		o.pkt = pkt
	}
	n := copy(p, o.pkt)
	o.pkt = o.pkt[n:]
	return n, nil
}

// markLastMetadata clears the last block flag on every metadata block and sets it
// on the final one. The flag describes a stream, and the stream the native reader
// is about to see is the one assembled here rather than the one the encoder
// wrote, so it is set from what was actually collected.
func markLastMetadata(native []byte) {
	if len(native) < 4 || string(native[:4]) != "fLaC" {
		return
	}
	last := -1
	for at := 4; at+4 <= len(native); {
		last = at
		native[at] &^= 0x80
		length := int(native[at+1])<<16 | int(native[at+2])<<8 | int(native[at+3])
		at += 4 + length
	}
	if last >= 0 {
		native[last] |= 0x80
	}
}

// openOggFlac reassembles a native FLAC stream out of an Ogg FLAC one.
//
// The first packet carries a mapping header, the native signature and the
// STREAMINFO block; the header count in it is the number of metadata packets
// that follow, not counting the first. Everything after those is audio, and the
// frames are already in the shape the native stream wants, so the packets are
// simply run together behind the reassembled header.
func openOggFlac(r io.Reader) (*Decoder, error) {
	pages := newOggPages(r)
	first, err := pages.next()
	if err != nil {
		return nil, err
	}
	if !prefix(first, "\x7fFLAC") || len(first) < 13 {
		return nil, errUnsupported
	}
	native := append([]byte(nil), first[9:]...)
	for i := 0; i < int(binary.BigEndian.Uint16(first[7:9])); i++ {
		pkt, err := pages.next()
		if err != nil {
			return nil, err
		}
		native = append(native, pkt...)
	}
	markLastMetadata(native)
	d, err := flacpcm.NewDecoder(&oggFlacReader{pages: pages, head: native})
	if err != nil {
		return nil, err
	}
	return newFLACDecoder(d)
}
