package pure

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	aacpcm "github.com/tphakala/go-aac/pcm"
	flacpcm "github.com/tphakala/go-flac/pcm"
	opuspcm "github.com/tphakala/go-opus/oggopus"
	opus "github.com/tphakala/go-opus/opus"
	wavpcm "github.com/tphakala/go-wav/pcm"
)

// Decoder is a container decoded to interleaved little-endian 16-bit PCM. Read
// yields whole frames only, so a caller can always interpret the bytes it gets
// as int16 without carrying a remainder.
type Decoder struct {
	r        io.Reader
	rate     int
	channels int
	closer   io.Closer
	// skip is how many samples at the source rate the container asks to be
	// dropped from the start, which is the encoder delay an edit list records.
	skip int
	// carry holds the tail of a frame that a source read stopped in the middle
	// of, so the next read can finish it.
	carry []byte
}

func (d *Decoder) Rate() int     { return d.rate }
func (d *Decoder) Channels() int { return d.channels }

// Skip is a leading run of source samples that carries no audio, such as an
// encoder's priming.
func (d *Decoder) Skip() int { return d.skip }

// Read returns interleaved PCM, whole frames at a time.
//
// A source hands back as many bytes as it happens to have, which need not be a
// whole number of frames once there are more than two channels: a six channel
// frame is twelve bytes, and no round buffer size is a multiple of twelve. A
// caller that dropped the tail of such a read would not just lose those samples,
// it would read every block after it with the channels rotated. So the tail of a
// frame is held back until the rest of it arrives.
func (d *Decoder) Read(p []byte) (int, error) {
	frame := d.channels * 2
	room := len(p) - len(p)%frame
	if room == 0 {
		return 0, io.ErrShortBuffer
	}
	n := copy(p, d.carry)
	d.carry = d.carry[n:]
	if n == room {
		return n, nil
	}
	got, err := d.r.Read(p[n:room])
	n += got
	whole := n - n%frame
	if whole < n {
		d.carry = append(d.carry[:0], p[whole:n]...)
	}
	if whole > 0 {
		// An error alongside data is reported by the call after this one, which
		// is what a reader that keeps returning it gives anyway.
		return whole, nil
	}
	return 0, err
}
func (d *Decoder) Close() error {
	if d.closer != nil {
		return d.closer.Close()
	}
	return nil
}

var errUnsupported = errors.New("pure: container is not handled in process")

// PeekSize is how far the container signature is looked for. Every format here
// states its identity in the first few dozen bytes, and Opus in Ogg names its
// codec 28 bytes into the first page.
const PeekSize = 1 << 10

// Detect reports whether Open can decode the container whose first bytes are
// head. It is deliberately conservative: a false negative costs a fall back to
// FFmpeg, while a false positive costs one wasted open.
func Detect(head []byte) bool {
	switch {
	case prefix(head, "OggS"):
		// Ogg is only a wrapper, so the codec inside it is what decides. Opus
		// names itself OpusHead, Vorbis and FLAC each carry an identification
		// header of their own.
		return looksVorbis(head) || looksOggFlac(head) || bytes.Contains(head, []byte("OpusHead"))
	case prefix(head, "fLaC"):
		return true
	case prefix(head, "RIFF") && len(head) >= 12 && string(head[8:12]) == "WAVE":
		return true
	case prefix(head, "ID3") || looksMP3(head):
		return true
	case looksADTS(head):
		return true
	case looksMP4(head):
		// AAC in an MP4, whichever side of the media the header is on. A header
		// at the end is read by seeking to it, which the source has to allow;
		// when it does not, the open fails and the dispatcher falls back.
		return true
	case looksWebM(head):
		// Only Opus in WebM or Matroska; Vorbis and a video only file take the
		// fall back.
		return bytes.Contains(head, []byte("A_OPUS"))
	}
	return false
}

// looksADTS reports an ADTS frame header, which is how a bare AAC stream carries
// its own framing. The two layer bits are zero, which is what separates it from
// an MP3 frame header; 0xF6 masks exactly the sync bits, the layer bits and the
// protection bit, so both protected and unprotected frames match.
func looksADTS(head []byte) bool {
	return len(head) >= 2 && head[0] == 0xFF && head[1]&0xF6 == 0xF0
}

// looksMP3 reports an MPEG audio frame header. The layer bits have to be set,
// which is what separates MP3 from the ADTS framing that AAC uses: both begin
// with eleven sync bits, and the AAC case belongs to FFmpeg.
func looksMP3(head []byte) bool {
	return len(head) >= 4 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 && head[1]&0x06 != 0
}

// Open reads the container signature and returns a decoder for the rest of the
// stream. The bytes it peeks are replayed to the decoder, so nothing is lost.
func Open(r io.Reader) (*Decoder, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, err := br.Peek(PeekSize)
	if err != nil && len(head) == 0 {
		return nil, err
	}
	// A container whose header sits at its end can only be read by a source that
	// can seek, so the capability is offered to the reader when it is there.
	var seeker io.ReadSeeker
	if s, ok := r.(io.ReadSeeker); ok {
		seeker = s
	}
	return open(head, br, seeker)
}

func open(head []byte, r io.Reader, seeker io.ReadSeeker) (*Decoder, error) {
	switch {
	case prefix(head, "OggS"):
		switch {
		case looksVorbis(head):
			v, err := oggvorbis.NewReader(r)
			if err != nil {
				return nil, err
			}
			return newDecoder(newVorbis(v), v.SampleRate(), v.Channels(), nil)
		case looksOggFlac(head):
			return openOggFlac(r)
		}
		d, err := opuspcm.NewDecoder(r)
		if err != nil {
			return nil, err
		}
		info := d.Info()
		// The Ogg Opus reader always decodes to 48 kHz, whatever source rate
		// OpusHead recorded.
		return newDecoder(d, int(info.OutputSampleRate), info.Channels, nil)
	case prefix(head, "fLaC"):
		d, err := flacpcm.NewDecoder(r)
		if err != nil {
			return nil, err
		}
		return newFLACDecoder(d)
	case prefix(head, "RIFF") && len(head) >= 12 && string(head[8:12]) == "WAVE":
		// WithConvertTo normalises PCM, float and companded WAV alike to the
		// signed 16-bit the pipeline works in.
		d, err := wavpcm.NewDecoder(r, wavpcm.WithConvertTo(16))
		if err != nil {
			return nil, err
		}
		info := d.Info()
		return newDecoder(d, info.SampleRate, info.Channels, nil)
	case looksADTS(head):
		// A bare AAC stream frames itself, so the reader decoder handles it and
		// no container parse is needed.
		d, err := aacpcm.NewDecoder(r)
		if err != nil {
			return nil, err
		}
		info := d.Info()
		return newDecoder(d, info.SampleRate, info.Channels, nil)
	case looksMP4(head):
		return openAnyMP4(head, r, seeker)
	case looksWebM(head):
		track, info, err := openWebM(r)
		if err != nil {
			return nil, err
		}
		// Opus always decodes at 48 kHz whatever rate it was encoded from.
		dec, err := opus.NewDecoder(OutRate, info.channels)
		if err != nil {
			return nil, err
		}
		rd := &webmReader{
			w:        track,
			dec:      dec,
			channels: info.channels,
			pcm:      make([]int16, opusFrameSamples*info.channels),
		}
		out, err := newDecoder(rd, OutRate, info.channels, nil)
		if err != nil {
			return nil, err
		}
		out.skip = info.preSkip
		return out, nil
	case prefix(head, "ID3") || looksMP3(head):
		d, err := mp3.NewDecoder(r)
		if err != nil {
			return nil, err
		}
		// go-mp3 always reports its output as stereo 16-bit.
		out, err := newDecoder(d, d.SampleRate(), 2, nil)
		if err != nil {
			return nil, err
		}
		out.skip = mp3Delay(head)
		return out, nil
	}
	return nil, errUnsupported
}

// mp3Delay is the encoder delay an MP3 asks to be dropped, in samples at the
// file's own rate.
//
// MP3 has nowhere to record this, so LAME, and the encoders that copied it,
// write the delay and the padding into the Xing or Info frame that sits at the
// front of the file. Nothing in the decoded audio marks where the real first
// sample is, so a decoder that does not read that frame hands back the run-up as
// sound and every MP3 starts about 26 ms late. go-mp3 does not read it, so it is
// read here.
func mp3Delay(head []byte) int {
	// The frame need not be the first thing in the file: an ID3v2 tag comes
	// first when there is one.
	at := 0
	if prefix(head, "ID3") {
		if len(head) < 10 {
			return 0
		}
		at = 10 + syncsafe(head[6:10])
		if head[5]&0x10 != 0 {
			at += 10 // the tag declares a footer
		}
	}
	if at+4 > len(head) || head[at] != 0xFF || head[at+1]&0xE0 != 0xE0 {
		return 0
	}
	// The tag sits after the frame header, after the side information and after
	// the two bytes of CRC a protected frame carries. Its size depends on the
	// MPEG version and on whether the frame is mono.
	mpeg1 := head[at+1]&0x18 == 0x18
	mono := head[at+3]&0xC0 == 0xC0
	var side int
	switch {
	case mpeg1 && mono:
		side = 17
	case mpeg1:
		side = 32
	case mono:
		side = 9
	default:
		side = 17
	}
	p := at + 4 + side
	if head[at+1]&0x01 == 0 {
		p += 2
	}
	if p+8 > len(head) || (string(head[p:p+4]) != "Xing" && string(head[p:p+4]) != "Info") {
		return 0 // a plain frame, so the file declares nothing
	}
	flags := int(binary.BigEndian.Uint32(head[p+4 : p+8]))
	p += 8
	for _, field := range []struct {
		bit    int
		length int
	}{{0x01, 4}, {0x02, 4}, {0x04, 100}, {0x08, 4}} {
		if flags&field.bit != 0 {
			p += field.length
		}
	}
	// The encoder's own tag follows: nine bytes of name and version, then the
	// delay and the padding, twelve bits each, twenty-one bytes in. LAME writes
	// it, and FFmpeg writes the same shape under its own name, so it is not the
	// name that identifies it but being text at all. A file carrying no such tag
	// leaves this area zeroed, which is what the check rejects.
	delay := 0
	if p+24 <= len(head) && printable(head[p:p+9]) {
		delay = int(head[p+21])<<4 | int(head[p+22])>>4
	}
	// Three things stand in front of the audio. The Xing frame is metadata, but
	// it is a valid frame, and a decoder that does not recognise it as metadata
	// decodes it like any other, so its silence counts. The encoder delay is the
	// run-up the encoder produced. The filterbank adds a delay of its own, which
	// belongs to Layer III rather than to the file.
	frame := 1152
	if !mpeg1 {
		frame = 576
	}
	return frame + delay + mp3DecoderDelay
}

// mp3DecoderDelay is the delay the Layer III synthesis filterbank introduces
// before any encoder delay. The first window has nothing to overlap with, so a
// window's worth of samples has to pass through before the output means
// anything. It is a property of the format, not of the file.
const mp3DecoderDelay = 529

// printable reports whether every byte is text, which is what an encoder's name
// and version are.
func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// syncsafe reads the four-byte size an ID3v2 tag records. Each byte keeps its
// top bit clear so the size can never be mistaken for a frame sync.
func syncsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// newFLACDecoder wraps a FLAC reader, converting the samples when the file was
// mastered at a depth other than 16 bits. The reader hands back the samples at
// the depth the stream declares, so anything wider would be read as noise if it
// were taken as 16-bit.
func newFLACDecoder(d *flacpcm.Decoder) (*Decoder, error) {
	info := d.Info()
	if info.BitDepth <= 0 || info.BitDepth > 32 {
		return nil, fmt.Errorf("pure: FLAC declares %d bits per sample", info.BitDepth)
	}
	var src io.Reader = d
	if info.BitDepth != 16 {
		src = newSamples(d, info.BitDepth)
	}
	return newDecoder(src, info.SampleRate, info.Channels, nil)
}

// newDecoder rejects the shapes the pipeline does not build a filter for. Discord
// carries stereo, so anything wider is folded down by the mix in downmix.go, and
// a layout wider than that mix covers falls back.
func newDecoder(r io.Reader, rate, channels int, closer io.Closer) (*Decoder, error) {
	if rate <= 0 {
		return nil, errors.New("pure: source declares no sample rate")
	}
	if channels < 1 || channels > maxChannels {
		return nil, fmt.Errorf("pure: %d audio channels", channels)
	}
	return &Decoder{r: r, rate: rate, channels: channels, closer: closer}, nil
}

func prefix(b []byte, s string) bool {
	return len(b) >= len(s) && string(b[:len(s)]) == s
}

// monoUpmix is the gain FFmpeg applies to each channel when it turns one channel
// into two. It is energy preserving rather than amplitude preserving, and the
// two engines have to agree or a mono track would change loudness depending on
// which one played it.
const monoUpmix = 0.7071067811865476

// toStereo converts interleaved PCM of any supported channel count into the
// interleaved stereo the encoder takes. Mono is upmixed by a fixed gain and
// stereo is copied; anything wider is folded down by the same mix FFmpeg builds,
// which is in downmix.go.
func toStereo(src []int16, channels int, dst []int16) []int16 {
	switch channels {
	case 2:
		return append(dst, src...)
	case 1:
		for _, v := range src {
			s := scale(v, monoUpmix)
			dst = append(dst, s, s)
		}
		return dst
	}
	return downmix(src, channels, dst)
}

// asInt16 reinterprets a little-endian PCM byte slice.
func asInt16(b []byte) []int16 {
	if len(b) < 2 {
		return nil
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// putInt16 writes interleaved PCM as the little-endian bytes the pipeline
// carries.
func putInt16(dst []byte, src []int16) []byte {
	for _, v := range src {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(v))
	}
	return dst
}

// opusFrameSamples is the longest frame Opus can carry, 120 ms at 48 kHz.
const opusFrameSamples = 5760

// webmReader decodes the Opus packets of a WebM stream into PCM. It is only used
// when the stream needs processing; a stream that does not is forwarded by
// passthrough, which never decodes anything.
type webmReader struct {
	w        *webm
	dec      *opus.Decoder
	channels int
	pcm      []int16
	scratch  []byte
	off      int
}

func (r *webmReader) Read(p []byte) (int, error) {
	for r.off >= len(r.scratch) {
		pkt, _, err := r.w.next()
		if err != nil {
			return 0, err
		}
		n, err := r.dec.Decode(pkt, r.pcm)
		if err != nil {
			return 0, err
		}
		r.scratch = putInt16(r.scratch[:0], r.pcm[:n*r.channels])
		r.off = 0
	}
	n := copy(p, r.scratch[r.off:])
	r.off += n
	return n, nil
}
