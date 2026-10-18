package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

const MaxEncodedTrack = 256 << 10

// Encode uses Lavaplayer's version 3 format and Java modified UTF-8.
// Source-specific bytes are intentionally absent for newly resolved tracks.
// Decode preserves the original encoded value, including source extensions.
func Encode(info TrackInfo) (*Track, error) {
	var b bytes.Buffer
	b.WriteByte(3)
	for _, s := range []string{info.Title, info.Author} {
		if err := writeUTF(&b, s); err != nil {
			return nil, err
		}
	}
	_ = binary.Write(&b, binary.BigEndian, info.Length)
	if err := writeUTF(&b, info.Identifier); err != nil {
		return nil, err
	}
	if info.IsStream {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	for _, s := range []*string{info.URI, info.ArtworkURL, info.ISRC} {
		if s == nil {
			b.WriteByte(0)
		} else {
			b.WriteByte(1)
			if err := writeUTF(&b, *s); err != nil {
				return nil, err
			}
		}
	}
	if err := writeUTF(&b, info.SourceName); err != nil {
		return nil, err
	}
	_ = binary.Write(&b, binary.BigEndian, info.Position)
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(b.Len())|1<<30)
	out.Write(b.Bytes())
	return &Track{Encoded: base64.StdEncoding.EncodeToString(out.Bytes()), Info: info, PluginInfo: Object{}, UserData: Object{}}, nil
}

func Decode(encoded string) (*Track, error) {
	bad := func() (*Track, error) { return nil, errors.New("invalid encoded track") }
	if len(encoded) > MaxEncodedTrack {
		return bad()
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(b) < 5 {
		return bad()
	}
	header := binary.BigEndian.Uint32(b)
	if header>>31 != 0 || int(header&0x3fffffff) != len(b)-4 {
		return bad()
	}
	r := bytes.NewReader(b[4:])
	version := byte(1)
	if header&(1<<30) != 0 {
		version, err = r.ReadByte()
		if err != nil || version < 1 || version > 3 {
			return bad()
		}
	}
	var t TrackInfo
	if t.Title, err = readUTF(r); err != nil {
		return bad()
	}
	if t.Author, err = readUTF(r); err != nil {
		return bad()
	}
	if binary.Read(r, binary.BigEndian, &t.Length) != nil || t.Length < 0 {
		return bad()
	}
	if t.Identifier, err = readUTF(r); err != nil {
		return bad()
	}
	v, err := r.ReadByte()
	if err != nil || v > 1 {
		return bad()
	}
	t.IsStream = v == 1
	t.IsSeekable = !t.IsStream
	for i, dest := range []**string{&t.URI, &t.ArtworkURL, &t.ISRC} {
		if (i == 0 && version < 2) || (i > 0 && version < 3) {
			continue
		}
		v, err = r.ReadByte()
		if err != nil || v > 1 {
			return bad()
		}
		if v == 1 {
			s, e := readUTF(r)
			if e != nil {
				return bad()
			}
			*dest = &s
		}
	}
	if t.SourceName, err = readUTF(r); err != nil || r.Len() < 8 {
		return bad()
	}
	// Lavaplayer appends source-specific data before the final position.
	_, _ = r.Seek(-8, io.SeekEnd)
	if binary.Read(r, binary.BigEndian, &t.Position) != nil || t.Position < 0 {
		return bad()
	}
	return &Track{Encoded: encoded, Info: t, PluginInfo: Object{}, UserData: Object{}}, nil
}

func writeUTF(w io.Writer, s string) error {
	buf := make([]byte, 0, len(s))
	for _, c := range utf16.Encode([]rune(s)) {
		switch {
		case c > 0 && c < 128:
			buf = append(buf, byte(c))
		case c < 2048:
			buf = append(buf, byte(0xc0|c>>6), byte(0x80|c&63))
		default:
			buf = append(buf, byte(0xe0|c>>12), byte(0x80|(c>>6)&63), byte(0x80|c&63))
		}
	}
	if len(buf) > 65535 {
		return fmt.Errorf("track text exceeds Java UTF limit")
	}
	if err := binary.Write(w, binary.BigEndian, uint16(len(buf))); err != nil {
		return err
	}
	_, err := w.Write(buf)
	return err
}

func readUTF(r *bytes.Reader) (string, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	u := make([]uint16, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		i++
		switch {
		case c > 0 && c < 128:
			u = append(u, uint16(c))
		case c&0xe0 == 0xc0:
			if i >= len(b) || b[i]&0xc0 != 0x80 {
				return "", io.ErrUnexpectedEOF
			}
			v := uint16(c&31)<<6 | uint16(b[i]&63)
			i++
			if v != 0 && v < 128 {
				return "", errors.New("overlong UTF")
			}
			u = append(u, v)
		case c&0xf0 == 0xe0:
			if i+1 >= len(b) || b[i]&0xc0 != 0x80 || b[i+1]&0xc0 != 0x80 {
				return "", io.ErrUnexpectedEOF
			}
			v := uint16(c&15)<<12 | uint16(b[i]&63)<<6 | uint16(b[i+1]&63)
			i += 2
			if v < 2048 {
				return "", errors.New("overlong UTF")
			}
			u = append(u, v)
		default:
			return "", errors.New("invalid modified UTF-8")
		}
	}
	return string(utf16.Decode(u)), nil
}
