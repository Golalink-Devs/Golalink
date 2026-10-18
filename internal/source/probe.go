package source

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"time"
)

// Probe summarises the audio found in the leading bytes of a container.
type Probe struct {
	// Audio reports that the container holds an audio track. A caller that sees
	// false rejects the URL.
	Audio bool
	// Duration is zero when the container does not state one. A live stream has
	// no duration, and neither does a container whose index sits past the window
	// read here.
	Duration time.Duration
	Title    string
	Artist   string
}

// maxProbe bounds how much of a container is read while looking for the index
// that carries the metadata. A streaming MP4 puts it before the audio, and every
// other format states its duration in the first few KiB.
const maxProbe = 8 << 20

// firstProbe is the window read before the format is known. Every header-only
// format answers from it, and keeping it small stops a live stream from stalling
// the load while the probe waits for bytes that are only there for MP4.
const firstProbe = 16 << 10

var errNoAudio = errors.New("URL contains no supported audio stream")

// window reads a container incrementally and keeps what it has read, so a parser
// can ask for more bytes without seeking back to the start.
type window struct {
	r    io.Reader
	buf  []byte
	eof  bool
	size int64
}

// need grows the buffered prefix to at least n bytes, reporting whether it got
// there. It never reads past maxProbe.
func (w *window) need(n int) bool {
	if n > maxProbe {
		n = maxProbe
	}
	for len(w.buf) < n && !w.eof {
		grow := 32 << 10
		if len(w.buf)+grow > maxProbe {
			grow = maxProbe - len(w.buf)
		}
		if grow <= 0 {
			break
		}
		start := len(w.buf)
		w.buf = append(w.buf, make([]byte, grow)...)
		m, err := w.r.Read(w.buf[start:])
		w.buf = w.buf[:start+m]
		if err != nil || m == 0 {
			// A reader that reports no progress is treated as finished, so a
			// misbehaving body cannot spin here.
			w.eof = true
		}
	}
	return len(w.buf) >= n
}

// ProbeContainer identifies the audio in a container from its leading bytes.
// size is the total length of the file, or a negative number when the server did
// not say; two formats can only estimate a duration when it is known.
//
// The returned error is the one callers show for a URL that is not audio.
func ProbeContainer(r io.Reader, size int64) (Probe, error) {
	w := &window{r: r, size: size}
	if !w.need(16) {
		return Probe{}, errNoAudio
	}
	b := w.buf
	switch {
	case hasPrefix(b, "ID3") || startsMPEGFrame(b):
		return probeMP3(w)
	case hasPrefix(b, "fLaC"):
		return probeFLAC(w)
	case hasPrefix(b, "OggS"):
		return probeOgg(w)
	case hasPrefix(b, "RIFF") && len(b) >= 12 && string(b[8:12]) == "WAVE":
		return probeWAV(w)
	case len(b) >= 4 && binary.BigEndian.Uint32(b) == 0x1A45DFA3:
		return probeMatroska(w)
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return probeMP4(w)
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		// ADTS AAC: the sync word and a layer field of zero.
		return probeADTS(w)
	}
	return Probe{}, errNoAudio
}

func hasPrefix(b []byte, s string) bool { return len(b) >= len(s) && string(b[:len(s)]) == s }

// text trims a decoded tag value of its terminator and padding.
func text(b []byte) string {
	return strings.TrimSpace(strings.Trim(string(b), "\x00"))
}

// ------------------------------------------------------------------ MP4 / M4A

// forEachBox walks one level of ISO base media boxes in order. A box whose
// declared size runs past the bytes read so far still contributes its head,
// which is where a movie header lives.
func forEachBox(b []byte, fn func(typ string, body []byte)) {
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		head := 8
		switch {
		case size == 1:
			if len(b) < 16 {
				return
			}
			full := binary.BigEndian.Uint64(b[8:16])
			if full > math.MaxInt32 {
				return
			}
			size, head = int(full), 16
		case size == 0:
			size = len(b)
		}
		if size < head {
			return
		}
		if size > len(b) {
			fn(typ, b[head:])
			return
		}
		fn(typ, b[head:size])
		b = b[size:]
	}
}

// findBox returns the body of the first box of the given type.
func findBox(b []byte, want string) ([]byte, bool) {
	var found []byte
	forEachBox(b, func(typ string, body []byte) {
		if found == nil && typ == want {
			found = body
		}
	})
	return found, found != nil
}

// boxSpan returns the body of the first box of the given type together with
// whether the bytes read so far hold all of it. A caller that needs every child
// box, rather than just the head, has to wait for the whole thing.
func boxSpan(b []byte, want string) (body []byte, complete, found bool) {
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		head := 8
		switch {
		case size == 1:
			if len(b) < 16 {
				return nil, false, false
			}
			full := binary.BigEndian.Uint64(b[8:16])
			if full > math.MaxInt32 {
				return nil, false, false
			}
			size, head = int(full), 16
		case size == 0:
			size = len(b)
		}
		if size < head {
			return nil, false, false
		}
		if typ == want {
			if size > len(b) {
				return b[head:], false, true
			}
			return b[head:size], true, true
		}
		if size > len(b) {
			return nil, false, false
		}
		b = b[size:]
	}
	return nil, false, false
}

func probeMP4(w *window) (Probe, error) {
	for {
		if moov, complete, found := boxSpan(w.buf, "moov"); found {
			if p, ok := parseMP4Movie(moov, complete); ok {
				return p, nil
			}
		}
		if w.eof || len(w.buf) >= maxProbe {
			break
		}
		next := len(w.buf) * 2
		if next < firstProbe {
			next = firstProbe
		}
		if next > maxProbe {
			next = maxProbe
		}
		w.need(next)
	}
	// ftyp is proof that this is a media container; without a reachable moov
	// there is simply no duration to report.
	return Probe{Audio: true}, nil
}

// parseMP4Movie reads the movie header, the track handlers and the iTunes tags.
// It reports false when the answer is not in yet, which is the case while the
// movie header is missing and while the track list has not been read far enough
// to rule audio out. The caller then widens the window and tries again.
func parseMP4Movie(moov []byte, complete bool) (Probe, bool) {
	mvhd, ok := findBox(moov, "mvhd")
	if !ok || len(mvhd) < 20 {
		return Probe{}, false
	}
	var p Probe
	if mvhd[0] == 1 {
		if len(mvhd) >= 32 {
			if timescale := binary.BigEndian.Uint32(mvhd[20:24]); timescale > 0 {
				if length := binary.BigEndian.Uint64(mvhd[24:32]); length > 0 && length != math.MaxUint64 {
					p.Duration = time.Duration(float64(length) / float64(timescale) * float64(time.Second))
				}
			}
		}
	} else if timescale := binary.BigEndian.Uint32(mvhd[12:16]); timescale > 0 {
		if length := binary.BigEndian.Uint32(mvhd[16:20]); length > 0 && length != math.MaxUint32 {
			p.Duration = time.Duration(float64(length) / float64(timescale) * float64(time.Second))
		}
	}
	// Every track entry names its handler, and 'soun' is the audio one.
	forEachBox(moov, func(typ string, body []byte) {
		if typ != "trak" {
			return
		}
		mdia, ok := findBox(body, "mdia")
		if !ok {
			return
		}
		if hdlr, ok := findBox(mdia, "hdlr"); ok && len(hdlr) >= 12 && string(hdlr[8:12]) == "soun" {
			p.Audio = true
		}
	})
	if !p.Audio {
		if !complete {
			// The track list runs past what was read, so audio cannot be ruled
			// out yet. A real song often has a movie header far larger than the
			// first window.
			return Probe{}, false
		}
		return p, true
	}
	readMP4Tags(moov, &p)
	return p, true
}

// readMP4Tags fills Title and Artist from ilst, which nests as
// moov.udta.meta.ilst.<©nam|©ART>.data.
func readMP4Tags(moov []byte, p *Probe) {
	udta, ok := findBox(moov, "udta")
	if !ok {
		return
	}
	meta, ok := findBox(udta, "meta")
	if !ok || len(meta) < 4 {
		return
	}
	ilst, ok := findBox(meta[4:], "ilst") // a version and flags word precedes the children
	if !ok {
		return
	}
	forEachBox(ilst, func(typ string, body []byte) {
		if typ != "\xa9nam" && typ != "\xa9ART" {
			return
		}
		data, ok := findBox(body, "data")
		if !ok || len(data) <= 8 {
			return
		}
		value := text(data[8:]) // a type indicator precedes the bytes
		if value == "" {
			return
		}
		if typ == "\xa9nam" && p.Title == "" {
			p.Title = value
		}
		if typ == "\xa9ART" && p.Artist == "" {
			p.Artist = value
		}
	})
}

// ---------------------------------------------------------------------- FLAC

func probeFLAC(w *window) (Probe, error) {
	// The metadata block chain starts at byte 4 and STREAMINFO is always first.
	var p Probe
	off := 4
	for {
		if !w.need(off + 4) {
			break
		}
		header := w.buf[off]
		last := header&0x80 != 0
		kind := header & 0x7F
		length := int(w.buf[off+1])<<16 | int(w.buf[off+2])<<8 | int(w.buf[off+3])
		if length < 0 || !w.need(off+4+length) {
			break
		}
		body := w.buf[off+4 : off+4+length]
		switch kind {
		case 0: // STREAMINFO
			if len(body) >= 18 {
				packed := binary.BigEndian.Uint64(body[10:18])
				rate := int(packed >> 44)
				total := int64(packed & 0xFFFFFFFFF)
				if rate > 0 && total > 0 {
					p.Duration = time.Duration(float64(total) / float64(rate) * float64(time.Second))
				}
				p.Audio = true
			}
		case 4: // VORBIS_COMMENT
			readVorbisComment(body, &p)
		}
		off += 4 + length
		if last {
			break
		}
	}
	if !p.Audio {
		return Probe{}, errNoAudio
	}
	return p, nil
}

// readVorbisComment fills Title and Artist from a comment list, whose lengths
// are little endian unlike the rest of the container.
func readVorbisComment(b []byte, p *Probe) {
	if len(b) < 8 {
		return
	}
	off := 4 + int(binary.LittleEndian.Uint32(b))
	if off+4 > len(b) {
		return
	}
	count := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	for i := 0; i < count && off+4 <= len(b); i++ {
		n := int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if n < 0 || off+n > len(b) {
			return
		}
		key, value, ok := strings.Cut(string(b[off:off+n]), "=")
		off += n
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToUpper(key) {
		case "TITLE":
			if p.Title == "" {
				p.Title = value
			}
		case "ARTIST", "ALBUMARTIST":
			if p.Artist == "" {
				p.Artist = value
			}
		}
	}
}

// readWhole pulls the rest of the file into the window when the server declared
// a length small enough to buffer, so a scanner can reach the end. It reports
// whether the window now holds the whole file.
func (w *window) readWhole() bool {
	if w.size < 0 || w.size > maxProbe {
		return false
	}
	w.need(int(w.size))
	return len(w.buf) >= int(w.size)
}

// ----------------------------------------------------------------------- Ogg

// probeOgg identifies the codec and, when the whole file fits the window, counts
// the samples the pages report. A live stream states its length nowhere but in
// the final page, which never arrives, so it stays a stream.
func probeOgg(w *window) (Probe, error) {
	// The identification and comment headers sit in the first pages, so one
	// window finds them; a longer file only matters for the granule scan.
	w.need(firstProbe)
	type codec struct {
		marker string
		rate   int
	}
	for _, c := range []codec{
		{"OpusHead", 48000}, // the Opus granule is always at 48 kHz
		{"\x01vorbis", 0},   // filled in from the identification header
		{"\x7FFLAC", 0},     // the FLAC granule needs STREAMINFO, left unknown
	} {
		i := bytes.Index(w.buf, []byte(c.marker))
		if i < 0 {
			continue
		}
		p := Probe{Audio: true}
		rate := c.rate
		if c.marker == "\x01vorbis" {
			rate = vorbisRate(w.buf[i+len(c.marker):])
		}
		// The comment header follows the identification header, in a later page.
		if j := bytes.Index(w.buf, []byte("OpusTags")); j >= 0 {
			readVorbisComment(w.buf[j+8:], &p) // past the eight byte signature
		}
		if j := bytes.Index(w.buf, []byte("\x03vorbis")); j >= 0 {
			readVorbisComment(w.buf[j+7:], &p) // past the packet type and signature
		}
		if rate > 0 && w.readWhole() {
			p.Duration = oggDuration(w.buf, rate)
		}
		return p, nil
	}
	return Probe{}, errNoAudio
}

// vorbisRate reads the sample rate out of a Vorbis identification packet, which
// holds a version, a channel count and then the rate.
func vorbisRate(b []byte) int {
	if len(b) < 9 {
		return 0
	}
	return int(binary.LittleEndian.Uint32(b[5:9]))
}

// oggDuration sums the final granule position, which every page states as the
// running sample count.
func oggDuration(b []byte, rate int) time.Duration {
	var last uint64
	for off := 0; off+27 <= len(b); {
		if string(b[off:off+4]) != "OggS" {
			break
		}
		segments := int(b[off+26])
		if off+27+segments > len(b) {
			break
		}
		size := 0
		for i := 0; i < segments; i++ {
			size += int(b[off+27+i])
		}
		end := off + 27 + segments + size
		if end > len(b) {
			break
		}
		// A page that completes no packet carries no position.
		if granule := binary.LittleEndian.Uint64(b[off+6 : off+14]); granule != math.MaxUint64 {
			last = granule
		}
		off = end
	}
	if last == 0 || rate <= 0 {
		return 0
	}
	return time.Duration(float64(last) / float64(rate) * float64(time.Second))
}

// ------------------------------------------------------------------ ADTS AAC

var adtsRates = [16]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350, 0, 0, 0}

// probeADTS counts the frames of a raw AAC stream. The header states the sample
// rate and each frame is 1024 samples, so a file that fits the window has an
// exact length; a live stream has none.
func probeADTS(w *window) (Probe, error) {
	p := Probe{Audio: true}
	if w.readWhole() {
		total, rate := 0, 0
		for off := 0; off+7 <= len(w.buf); {
			if w.buf[off] != 0xFF || w.buf[off+1]&0xF6 != 0xF0 {
				break
			}
			index := int(w.buf[off+2]>>2) & 0x0F
			if adtsRates[index] == 0 {
				break
			}
			if rate == 0 {
				rate = adtsRates[index]
			}
			length := int(w.buf[off+3]&0x03)<<11 | int(w.buf[off+4])<<3 | int(w.buf[off+5]>>5)
			if length < 7 || off+length > len(w.buf) {
				break
			}
			total += 1024
			off += length
		}
		if rate > 0 && total > 0 {
			p.Duration = time.Duration(float64(total) / float64(rate) * float64(time.Second))
		}
	}
	return p, nil
}

// ----------------------------------------------------------------------- WAV

func probeWAV(w *window) (Probe, error) {
	var p Probe
	byteRate, dataSize := 0, int64(-1)
	off := 12
	for {
		if !w.need(off + 8) {
			break
		}
		id := string(w.buf[off : off+4])
		size := int(binary.LittleEndian.Uint32(w.buf[off+4 : off+8]))
		if size < 0 {
			break
		}
		body := off + 8
		switch id {
		case "fmt ":
			if w.need(body + 16) {
				byteRate = int(binary.LittleEndian.Uint32(w.buf[body+8 : body+12]))
				p.Audio = true
			}
		case "data":
			dataSize = int64(size)
		case "LIST":
			if w.need(body + size) {
				readRIFFInfo(w.buf[body:body+size], &p)
			}
		}
		next := body + size
		if size%2 != 0 {
			next++ // chunks are word aligned
		}
		if next <= off {
			break
		}
		off = next
	}
	if !p.Audio {
		return Probe{}, errNoAudio
	}
	if byteRate > 0 {
		if dataSize < 0 && w.size > int64(off) {
			// A streamed WAV has no data size yet; the file size stands in.
			dataSize = w.size - int64(off)
		}
		if dataSize > 0 {
			p.Duration = time.Duration(float64(dataSize) / float64(byteRate) * float64(time.Second))
		}
	}
	return p, nil
}

// readRIFFInfo reads a LIST/INFO block, whose entries are INAM and IART.
func readRIFFInfo(b []byte, p *Probe) {
	if len(b) < 4 || string(b[:4]) != "INFO" {
		return
	}
	for off := 4; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		if size < 0 || off+8+size > len(b) {
			return
		}
		value := text(b[off+8 : off+8+size])
		switch id {
		case "INAM":
			if p.Title == "" {
				p.Title = value
			}
		case "IART":
			if p.Artist == "" {
				p.Artist = value
			}
		}
		off += 8 + size
		if size%2 != 0 {
			off++
		}
	}
}

// ----------------------------------------------------------------------- MP3

func startsMPEGFrame(b []byte) bool {
	_, ok := parseMP3Header(b)
	return ok
}

type mp3Header struct {
	version  int // 1, 2 or 25
	bitrate  int // bits per second
	rate     int
	padding  int
	channels int
	samples  int
	length   int
}

var mp3Rates = map[int][3]int{
	1:  {44100, 48000, 32000},
	2:  {22050, 24000, 16000},
	25: {11025, 12000, 8000},
}

var mp3Bitrates = map[int][16]int{
	1:  {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	2:  {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
	25: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
}

// parseMP3Header decodes a Layer III frame header, which is the only layer any
// source here serves.
func parseMP3Header(b []byte) (mp3Header, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return mp3Header{}, false
	}
	var h mp3Header
	switch (b[1] >> 3) & 3 {
	case 3:
		h.version = 1
	case 2:
		h.version = 2
	case 0:
		h.version = 25
	default:
		return mp3Header{}, false
	}
	if (b[1]>>1)&3 != 1 { // Layer III
		return mp3Header{}, false
	}
	index := int(b[2] >> 4)
	if index == 0 || index == 15 { // free, or not a valid bitrate
		return mp3Header{}, false
	}
	h.bitrate = mp3Bitrates[h.version][index] * 1000
	rateIndex := int(b[2]>>2) & 3
	if rateIndex == 3 {
		return mp3Header{}, false
	}
	h.rate = mp3Rates[h.version][rateIndex]
	h.padding = int(b[2]>>1) & 1
	if (b[3]>>6)&3 == 3 {
		h.channels = 1
	} else {
		h.channels = 2
	}
	if h.version == 1 {
		h.samples = 1152
	} else {
		h.samples = 576
	}
	h.length = h.samples/8*h.bitrate/h.rate + h.padding
	return h, h.rate > 0 && h.length > 0
}

func probeMP3(w *window) (Probe, error) {
	var p Probe
	start := 0
	if hasPrefix(w.buf, "ID3") {
		if !w.need(10) {
			return Probe{}, errNoAudio
		}
		total := 10 + synchsafe(w.buf[6:10])
		if w.buf[5]&0x10 != 0 {
			total += 10 // footer
		}
		if w.need(total) {
			readID3(w.buf[:total], &p)
		}
		start = total
	}
	// An ID3 tag alone is proof of an MP3, even if no frame arrived yet.
	p.Audio = true
	if !w.need(start + 4) {
		return p, nil
	}
	h, ok := parseMP3Header(w.buf[start:])
	if !ok {
		return p, nil
	}
	// A Xing or Info header states the frame count outright.
	side := 32
	switch {
	case h.version != 1 && h.channels == 1:
		side = 9
	case h.version != 1:
		side = 17
	case h.channels == 1:
		side = 17
	}
	at := start + 4 + side
	if w.need(at + 12) {
		if marker := string(w.buf[at : at+4]); marker == "Xing" || marker == "Info" {
			flags := binary.BigEndian.Uint32(w.buf[at+4 : at+8])
			if flags&1 != 0 {
				if frames := binary.BigEndian.Uint32(w.buf[at+8 : at+12]); frames > 0 {
					seconds := float64(frames) * float64(h.samples) / float64(h.rate)
					p.Duration = time.Duration(seconds * float64(time.Second))
				}
			}
		}
	}
	// Without a frame count, the file size and the first frame's bitrate give a
	// close estimate. It is only available when the server stated a length.
	if p.Duration == 0 && w.size > 0 && h.bitrate > 0 {
		if audio := w.size - int64(start); audio > 0 {
			p.Duration = time.Duration(float64(audio) * 8 / float64(h.bitrate) * float64(time.Second))
		}
	}
	return p, nil
}

// synchsafe decodes the four seven-bit bytes ID3 uses for lengths.
func synchsafe(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// readID3 reads the TIT2 and TPE1 frames of an ID3v2 tag.
func readID3(b []byte, p *Probe) {
	if len(b) < 10 || string(b[:3]) != "ID3" {
		return
	}
	off := 10
	if b[5]&0x40 != 0 { // an extended header precedes the frames
		if len(b) < off+4 {
			return
		}
		off += 4 + synchsafe(b[off:off+4])
	}
	for off+10 <= len(b) {
		id := string(b[off : off+4])
		if id == "\x00\x00\x00\x00" {
			return
		}
		size := synchsafe(b[off+4 : off+8])
		if size <= 0 || off+10+size > len(b) {
			return
		}
		body := b[off+10 : off+10+size]
		if len(body) > 1 {
			body = body[1:] // the text encoding byte
		}
		switch id {
		case "TIT2":
			if v := decodeID3Text(body); v != "" && p.Title == "" {
				p.Title = v
			}
		case "TPE1":
			if v := decodeID3Text(body); v != "" && p.Artist == "" {
				p.Artist = v
			}
		}
		off += 10 + size
	}
}

// decodeID3Text reads a frame body as UTF-16 when it carries a byte order mark
// and as Latin-1 or UTF-8 otherwise.
func decodeID3Text(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return text(utf16String(b[2:], binary.LittleEndian))
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return text(utf16String(b[2:], binary.BigEndian))
	}
	return text(b)
}

func utf16String(b []byte, order binary.ByteOrder) []byte {
	var out []byte
	for i := 0; i+1 < len(b); i += 2 {
		r := rune(order.Uint16(b[i : i+2]))
		if r == 0 {
			break
		}
		out = append(out, []byte(string(r))...)
	}
	return out
}

// ------------------------------------------------------------------ Matroska

// The EBML element identifiers this prober reads.
const (
	idSegment    = 0x18538067
	idInfo       = 0x1549A966
	idTitle      = 0x7BA9
	idTimecode   = 0x2AD7B1
	idDuration   = 0x4489
	idTracks     = 0x1654AE6B
	idTrackEntry = 0xAE
	idTrackType  = 0x83
	idTags       = 0x1254C367
	idTag        = 0x7373
	idSimpleTag  = 0x67C8
	idTagName    = 0x45A3
	idTagString  = 0x4487
	trackTypeAud = 2
)

func ebmlID(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	var n int
	for n = 1; n <= 4; n++ {
		if b[0]&(0x80>>(n-1)) != 0 {
			break
		}
	}
	if n > 4 || len(b) < n {
		return 0, 0
	}
	var id uint64
	for i := 0; i < n; i++ {
		id = id<<8 | uint64(b[i])
	}
	return id, n
}

func ebmlSize(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	var n int
	for n = 1; n <= 8; n++ {
		if b[0]&(0x80>>(n-1)) != 0 {
			break
		}
	}
	if n > 8 || len(b) < n {
		return 0, 0, false
	}
	value := uint64(b[0] &^ (0x80 >> (n - 1)))
	for i := 1; i < n; i++ {
		value = value<<8 | uint64(b[i])
	}
	return value, n, true
}

// probeMatroska walks the EBML tree for the segment information, the track
// types and the tag list.
func probeMatroska(w *window) (Probe, error) {
	w.need(firstProbe)
	var p Probe
	timecode := int64(1000000) // nanoseconds per tick when TimecodeScale is absent
	probeEBML(w.buf, &p, &timecode, 0)
	if !p.Audio {
		return Probe{}, errNoAudio
	}
	return p, nil
}

func probeEBML(b []byte, p *Probe, timecode *int64, depth int) {
	if depth > 6 {
		return
	}
	tagName := ""
	for off := 0; off < len(b); {
		id, idLen := ebmlID(b[off:])
		if idLen == 0 {
			return
		}
		size, sizeLen, ok := ebmlSize(b[off+idLen:])
		if !ok {
			return
		}
		head := idLen + sizeLen
		if off+head > len(b) {
			return
		}
		body := b[off+head:]
		if size > uint64(len(body)) {
			size = uint64(len(body)) // a truncated element still has a usable head
		}
		body = body[:size]
		switch id {
		case idSegment, idInfo, idTracks, idTrackEntry, idTags, idTag, idSimpleTag:
			tagName = ""
			probeEBML(body, p, timecode, depth+1)
		case idTitle:
			// The segment title stands in when no TITLE tag is present.
			if p.Title == "" {
				p.Title = text(body)
			}
		case idTimecode:
			if v, ok := ebmlUint(body); ok && v > 0 {
				*timecode = int64(v)
			}
		case idDuration:
			if v, ok := ebmlFloat(body); ok && v > 0 && *timecode > 0 {
				p.Duration = time.Duration(v * float64(*timecode))
			}
		case idTrackType:
			if v, ok := ebmlUint(body); ok && v == trackTypeAud {
				p.Audio = true
			}
		case idTagName:
			tagName = strings.ToUpper(text(body))
		case idTagString:
			value := text(body)
			switch tagName {
			case "TITLE":
				if p.Title == "" {
					p.Title = value
				}
			case "ARTIST", "ALBUM_ARTIST":
				if p.Artist == "" {
					p.Artist = value
				}
			}
		}
		off += head + int(size)
	}
}

func ebmlUint(b []byte) (uint64, bool) {
	if len(b) == 0 || len(b) > 8 {
		return 0, false
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, true
}

func ebmlFloat(b []byte) (float64, bool) {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), true
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), true
	}
	return 0, false
}
