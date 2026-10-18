package pure

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	aacpcm "github.com/tphakala/go-aac/pcm"
)

// The MP4 reader here exists because a progressive file whose moov box precedes
// its mdat can be read in a single forward pass: the header that describes every
// sample is at the front, so playback starts as soon as it has been read and a
// pipe never has to be seeked. That layout is what streaming services ship, and
// it is checked before this reader is used. A file with the moov at the end
// needs random access and is left to FFmpeg.

var (
	errMoovAfterMedia = errors.New("pure: mp4 has its moov box after the media")
	errNoAudioTrack   = errors.New("pure: mp4 has no audio track")
	errNoASC          = errors.New("pure: mp4 audio track has no AudioSpecificConfig")
	errNoSamples      = errors.New("pure: mp4 audio track has no samples")
	errFragmented     = errors.New("pure: mp4 keeps its samples in fragments and the source cannot seek")
	errHeaderTooBig   = errors.New("pure: mp4 header is unreasonably large")
)

// maxHeader bounds how much of the file before the media is buffered. A moov
// that describes a whole film is still far below this; anything larger is a file
// this reader should not be trying to handle.
const maxHeader = 16 << 20

// maxSamples bounds the sample table a header may declare, so a corrupt stsz
// cannot ask for an unbounded allocation.
const maxSamples = 16 << 20

// mp4Audio reads the audio track of a progressive MP4 that has already been
// positioned at the first byte of its media data.
type mp4Audio struct {
	r io.Reader
	// consumed is the file offset of the next byte r will return.
	consumed int64
	// sizes and offsets are parallel: the nth audio sample starts at offsets[n]
	// and is sizes[n] bytes long.
	sizes   []uint32
	offsets []int64
	next    int
	// desc is the DecoderSpecificInfo that describes the track.
	desc []byte
	// priming is the encoder delay the track asks to be dropped, in samples at
	// the track's own rate.
	priming int
}

// sample returns the next access unit, skipping over anything between samples.
// Video interleaved with the audio is skipped this way, so a muxed file works
// exactly like an audio-only one.
func (m *mp4Audio) sample() ([]byte, error) {
	if m.next >= len(m.sizes) {
		return nil, io.EOF
	}
	target := m.offsets[m.next]
	if target < m.consumed {
		// The media is not laid out in file order, which this forward reader
		// cannot follow.
		return nil, fmt.Errorf("pure: mp4 sample %d is behind the read position", m.next)
	}
	if skip := target - m.consumed; skip > 0 {
		n, err := io.CopyN(io.Discard, m.r, skip)
		m.consumed += n
		if err != nil {
			return nil, err
		}
	}
	size := int(m.sizes[m.next])
	au := make([]byte, size)
	if _, err := io.ReadFull(m.r, au); err != nil {
		return nil, err
	}
	m.consumed += int64(size)
	m.next++
	return au, nil
}

// openMP4 walks the top level boxes, keeps the moov and stops at the media. The
// reader is left positioned at the first byte of the mdat payload.
func openMP4(r io.Reader, seeker io.ReadSeeker) (*mp4Audio, error) {
	var moov []byte
	var consumed int64
	for {
		typ, size, header, err := readBox(r)
		if err != nil {
			return nil, err
		}
		consumed += header
		body := size - header
		if body < 0 {
			return nil, errHeaderTooBig
		}
		switch typ {
		case "moov":
			// Only a box that is held in memory is bounded. The media is as large
			// as the track is long and is streamed past rather than read, so its
			// size says nothing about whether this reader can cope with the file.
			if body > maxHeader {
				return nil, errHeaderTooBig
			}
			buf := make([]byte, body)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, err
			}
			consumed += body
			moov = buf
			if findBox(moov, "mvex") != nil {
				// A fragmented file says in its header that the samples are not
				// there, so the file has to be walked again to find them.
				if seeker == nil {
					return nil, errFragmented
				}
				return fragmentedMP4(seeker, moov)
			}
		case "mdat":
			if moov == nil {
				return nil, errMoovAfterMedia
			}
			return newMP4Audio(r, consumed, moov)
		default:
			if _, err := io.CopyN(io.Discard, r, body); err != nil {
				return nil, err
			}
			consumed += body
		}
	}
}

// openAnyMP4 reads an MP4 whichever side of the media its header is on. A header
// that comes first needs no more than a forward read. A header at the end needs
// random access, so it is only reachable when the body can seek; a body that
// cannot still plays the file, just not through here.
func openAnyMP4(head []byte, r io.Reader, seeker io.ReadSeeker) (*Decoder, error) {
	switch mp4LayoutOf(head) {
	case mp4HeaderFirst:
		m, err := openMP4(r, seeker)
		if err != nil {
			return nil, err
		}
		return aacFromMP4(m)
	case mp4HeaderLast:
		if seeker == nil {
			return nil, errMoovAfterMedia
		}
		m, err := openMP4Last(seeker)
		if err != nil {
			return nil, err
		}
		return aacFromMP4(m)
	}
	// The peek ended before either box did, so read forward and fall back to
	// seeking when the header turns out to be at the end. The reader may already
	// have been consumed by then, which is why the seeker is used instead.
	m, err := openMP4(r, seeker)
	if err == nil {
		return aacFromMP4(m)
	}
	if !errors.Is(err, errMoovAfterMedia) || seeker == nil {
		return nil, err
	}
	m, err = openMP4Last(seeker)
	if err != nil {
		return nil, err
	}
	return aacFromMP4(m)
}

// aacFromMP4 builds the decoder for an AAC track whose sample tables have been
// read. The configuration is parsed before any sample is touched, which is what
// names the reason for a track this decoder cannot take, such as HE-AAC.
func aacFromMP4(m *mp4Audio) (*Decoder, error) {
	asc, err := aacpcm.ParseASC(m.desc)
	if err != nil {
		return nil, err
	}
	dec, err := aacpcm.NewRawDecoder(m.desc)
	if err != nil {
		return nil, err
	}
	out, err := newDecoder(&mp4Reader{audio: m, dec: dec}, asc.SampleRate, asc.Channels, nil)
	if err != nil {
		return nil, err
	}
	out.skip = m.priming
	return out, nil
}

// newMP4Audio reads the sample tables for the audio track out of the moov box.
func newMP4Audio(r io.Reader, consumed int64, moov []byte) (*mp4Audio, error) {
	trak := audioTrack(moov)
	if trak == nil {
		return nil, errNoAudioTrack
	}
	mdia := findBox(trak, "mdia")
	stbl := findBox(findBox(mdia, "minf"), "stbl")
	if stbl == nil {
		return nil, errNoAudioTrack
	}
	desc := esdsConfig(stbl)
	if len(desc) == 0 {
		return nil, errNoASC
	}
	sizes := stszSizes(stbl)
	chunks := chunkOffsets(stbl)
	if len(sizes) == 0 || len(chunks) == 0 {
		return nil, errNoSamples
	}
	if len(sizes) > maxSamples {
		return nil, errNoSamples
	}
	offsets := sampleOffsets(sizes, chunks, stscRuns(stbl))
	if offsets == nil {
		return nil, errNoSamples
	}
	return &mp4Audio{
		r:        r,
		consumed: consumed,
		sizes:    sizes,
		offsets:  offsets,
		desc:     desc,
		priming:  editPriming(trak, mdia, desc),
	}, nil
}

// audioTrack finds the first track whose handler says sound.
func audioTrack(moov []byte) []byte {
	for _, trak := range boxesOf(moov, "trak") {
		mdia := findBox(trak, "mdia")
		if mdia == nil {
			continue
		}
		// The handler box starts with a version and flags, then a predefined
		// field, and then the four character handler type.
		hdlr := findBox(mdia, "hdlr")
		if len(hdlr) >= 12 && string(hdlr[8:12]) == "soun" {
			return trak
		}
	}
	return nil
}

// trackID reads the track identifier from a track header. A fragmented file
// names the track in every fragment rather than in a sample table, so the number
// is how a fragment's samples are matched to the track they belong to.
func trackID(trak []byte) uint32 {
	tkhd := findBox(trak, "tkhd")
	if len(tkhd) < 16 {
		return 0
	}
	// The identifier sits after the creation and modification times, which are
	// twice as wide in a version 1 header.
	if tkhd[0] == 1 {
		if len(tkhd) < 24 {
			return 0
		}
		return binary.BigEndian.Uint32(tkhd[20:24])
	}
	return binary.BigEndian.Uint32(tkhd[12:16])
}

// editPriming is the encoder delay an edit list records, converted to samples at
// the track's own rate. An encoder cannot emit its first real sample until it
// has filled its window, so the file marks the samples before that as priming
// and dropping them is what makes a track start exactly where it should.
func editPriming(trak, mdia, desc []byte) int {
	elst := findBox(findBox(trak, "edts"), "elst")
	if len(elst) < 8 || binary.BigEndian.Uint32(elst[4:8]) == 0 {
		return 0
	}
	var mediaTime int64
	if elst[0] == 1 {
		if len(elst) < 24 {
			return 0
		}
		mediaTime = int64(binary.BigEndian.Uint64(elst[16:24]))
	} else {
		if len(elst) < 16 {
			return 0
		}
		mediaTime = int64(int32(binary.BigEndian.Uint32(elst[12:16])))
	}
	if mediaTime <= 0 {
		return 0
	}
	// The offset is in the media timescale, which for audio is normally the
	// sample rate but is not required to be.
	rate, err := aacpcm.ParseASC(desc)
	if err != nil {
		return 0
	}
	scale := int64(rate.SampleRate)
	if ts := mediaTimescale(mdia); ts > 0 {
		mediaTime = mediaTime * scale / ts
	}
	// A delay of a minute or more means the box was not what it looked like.
	if mediaTime <= 0 || mediaTime > scale*60 {
		return 0
	}
	return int(mediaTime)
}

// mediaTimescale reads the media timescale from the media header.
func mediaTimescale(mdia []byte) int64 {
	mdhd := findBox(mdia, "mdhd")
	if len(mdhd) < 20 {
		return 0
	}
	if mdhd[0] == 1 {
		if len(mdhd) < 24 {
			return 0
		}
		return int64(binary.BigEndian.Uint32(mdhd[20:24]))
	}
	return int64(binary.BigEndian.Uint32(mdhd[12:16]))
}

func findBox(b []byte, typ string) []byte {
	if b == nil {
		return nil
	}
	for len(b) >= 8 {
		size := int64(binary.BigEndian.Uint32(b[0:4]))
		got := string(b[4:8])
		header := int64(8)
		if size == 1 {
			if len(b) < 16 {
				return nil
			}
			size = int64(binary.BigEndian.Uint64(b[8:16]))
			header = 16
		} else if size == 0 {
			size = int64(len(b))
		}
		if size < header || size > int64(len(b)) {
			return nil
		}
		if got == typ {
			return b[header:size]
		}
		b = b[size:]
	}
	return nil
}

func boxesOf(b []byte, typ string) [][]byte {
	var out [][]byte
	for len(b) >= 8 {
		size := int64(binary.BigEndian.Uint32(b[0:4]))
		got := string(b[4:8])
		header := int64(8)
		if size == 1 {
			if len(b) < 16 {
				return out
			}
			size = int64(binary.BigEndian.Uint64(b[8:16]))
			header = 16
		} else if size == 0 {
			size = int64(len(b))
		}
		if size < header || size > int64(len(b)) {
			return out
		}
		if got == typ {
			out = append(out, b[header:size])
		}
		b = b[size:]
	}
	return out
}

// esdsConfig pulls the AudioSpecificConfig out of the audio sample description.
// stsd holds a version and flags, an entry count, and then one sample entry per
// description; the esds box is a child of that entry, after the fixed audio
// fields.
func esdsConfig(stbl []byte) []byte {
	stsd := findBox(stbl, "stsd")
	if len(stsd) < 8 {
		return nil
	}
	entry := stsd[8:]
	if len(entry) < 36 {
		return nil
	}
	size := int64(binary.BigEndian.Uint32(entry[0:4]))
	if size < 36 || size > int64(len(entry)) {
		return nil
	}
	// A version 0 audio sample entry spends 28 bytes on itself after the eight
	// byte box header, and any child boxes follow.
	if format := string(entry[4:8]); format != "mp4a" {
		return nil
	}
	children := entry[36:size]
	esds := findBox(children, "esds")
	if len(esds) < 4 {
		return nil
	}
	// The esds payload opens with a version and flags, then the descriptor chain.
	return descriptors(esds[4:], 0x05)
}

// descriptors walks a chain of MPEG-4 descriptors and returns the payload of the
// first one carrying the wanted tag. The ES and decoder configuration
// descriptors nest, and each spends a fixed number of bytes on its own fields
// before its children begin.
func descriptors(b []byte, want byte) []byte {
	for len(b) >= 2 {
		tag := b[0]
		size, n := descriptorLen(b[1:])
		if n == 0 {
			return nil
		}
		end := 1 + n + size
		if end > len(b) {
			end = len(b)
		}
		body := b[1+n : end]
		if tag == want {
			return body
		}
		skip := 0
		switch tag {
		case 0x03: // ES_Descriptor: ES_ID and flags
			skip = 3
		case 0x04: // DecoderConfigDescriptor: object type through average bitrate
			skip = 13
		}
		if skip > 0 && len(body) > skip {
			if got := descriptors(body[skip:], want); got != nil {
				return got
			}
		}
		if end <= 1+n {
			return nil
		}
		b = b[end:]
	}
	return nil
}

// descriptorLen reads the variable length size that precedes a descriptor body,
// where every byte but the last has its top bit set.
func descriptorLen(b []byte) (int, int) {
	size := 0
	for i := 0; i < 4 && i < len(b); i++ {
		size = size<<7 | int(b[i]&0x7F)
		if b[i]&0x80 == 0 {
			return size, i + 1
		}
	}
	return 0, 0
}

// stszSizes reads the per sample sizes. A non zero uniform size means every
// sample is that long, which is what CBR AAC uses.
func stszSizes(stbl []byte) []uint32 {
	b := findBox(stbl, "stsz")
	if len(b) < 12 {
		return nil
	}
	uniform := binary.BigEndian.Uint32(b[4:8])
	count := binary.BigEndian.Uint32(b[8:12])
	if count > maxSamples {
		return nil
	}
	out := make([]uint32, count)
	if uniform != 0 {
		for i := range out {
			out[i] = uniform
		}
		return out
	}
	if len(b) < int(12+count*4) {
		return nil
	}
	for i := range out {
		out[i] = binary.BigEndian.Uint32(b[12+i*4:])
	}
	return out
}

// chunkOffsets reads the absolute file offset of each chunk from stco, or from
// its 64 bit form co64.
func chunkOffsets(stbl []byte) []int64 {
	if b := findBox(stbl, "stco"); len(b) >= 8 {
		count := binary.BigEndian.Uint32(b[4:8])
		if count > maxSamples || len(b) < int(8+count*4) {
			return nil
		}
		out := make([]int64, count)
		for i := range out {
			out[i] = int64(binary.BigEndian.Uint32(b[8+i*4:]))
		}
		return out
	}
	if b := findBox(stbl, "co64"); len(b) >= 8 {
		count := binary.BigEndian.Uint32(b[4:8])
		if count > maxSamples || len(b) < int(8+count*8) {
			return nil
		}
		out := make([]int64, count)
		for i := range out {
			out[i] = int64(binary.BigEndian.Uint64(b[8+i*8:]))
		}
		return out
	}
	return nil
}

// stscRun is one sample to chunk entry: from firstChunk onwards, each chunk
// holds samples samples.
type stscRun struct {
	firstChunk uint32
	samples    uint32
}

func stscRuns(stbl []byte) []stscRun {
	b := findBox(stbl, "stsc")
	if len(b) < 8 {
		return nil
	}
	count := binary.BigEndian.Uint32(b[4:8])
	if count > maxSamples || len(b) < int(8+count*12) {
		return nil
	}
	out := make([]stscRun, count)
	for i := range out {
		out[i] = stscRun{
			firstChunk: binary.BigEndian.Uint32(b[8+i*12:]),
			samples:    binary.BigEndian.Uint32(b[12+i*12:]),
		}
	}
	return out
}

// sampleOffsets expands the chunk table and the sample sizes into one file
// offset per sample.
func sampleOffsets(sizes []uint32, chunks []int64, runs []stscRun) []int64 {
	out := make([]int64, len(sizes))
	sample := 0
	run := 0
	for c := 0; c < len(chunks) && sample < len(sizes); c++ {
		// A run covers every chunk from its firstChunk until the next run begins.
		for run+1 < len(runs) && int(runs[run+1].firstChunk) <= c+1 {
			run++
		}
		per := uint32(1)
		if len(runs) > 0 {
			per = runs[run].samples
		}
		off := chunks[c]
		for i := uint32(0); i < per && sample < len(sizes); i++ {
			out[sample] = off
			off += int64(sizes[sample])
			sample++
		}
	}
	if sample < len(sizes) {
		return nil
	}
	return out
}

// readBox reads one box header and reports its type, its total size and the
// number of header bytes it consumed.
func readBox(r io.Reader) (typ string, size, header int64, err error) {
	var buf [16]byte
	if _, err = io.ReadFull(r, buf[:8]); err != nil {
		return "", 0, 0, err
	}
	size = int64(binary.BigEndian.Uint32(buf[0:4]))
	typ = string(buf[4:8])
	header = 8
	switch size {
	case 0:
		return "", 0, 0, errors.New("pure: mp4 box runs to the end of the file")
	case 1:
		if _, err = io.ReadFull(r, buf[8:16]); err != nil {
			return "", 0, 0, err
		}
		size = int64(binary.BigEndian.Uint64(buf[8:16]))
		header = 16
	}
	if size < header {
		return "", 0, 0, errors.New("pure: mp4 box has an impossible size")
	}
	return typ, size, header, nil
}

// mp4Reader turns the access units of an audio track into interleaved PCM.
type mp4Reader struct {
	audio   *mp4Audio
	dec     *aacpcm.FrameDecoder
	scratch []byte
	off     int
}

func (m *mp4Reader) Read(p []byte) (int, error) {
	for m.off >= len(m.scratch) {
		au, err := m.audio.sample()
		if err != nil {
			return 0, err
		}
		out, _, err := m.dec.DecodeFrame(m.scratch[:0], au)
		if err != nil {
			return 0, err
		}
		m.scratch, m.off = out, 0
	}
	n := copy(p, m.scratch[m.off:])
	m.off += n
	return n, nil
}

// mp4Layout describes where an MP4 keeps its header relative to its media.
type mp4Layout int

const (
	// mp4HeaderUnknown means the peek ended before either box did, so the file
	// has to be read to find out.
	mp4HeaderUnknown mp4Layout = iota
	mp4HeaderFirst
	mp4HeaderLast
)

// looksMP4 reports the file type box that opens every MP4.
func looksMP4(head []byte) bool {
	return len(head) >= 8 && string(head[4:8]) == "ftyp"
}

// mp4LayoutOf walks the top level boxes and reports which side of the media the
// header is on. A stream built for playback puts a small moov first, which one
// pass can follow; a file written for download puts the media first and the moov
// last, which takes a seek to reach.
func mp4LayoutOf(head []byte) mp4Layout {
	if !looksMP4(head) {
		return mp4HeaderUnknown
	}
	off := int64(binary.BigEndian.Uint32(head[0:4]))
	if off < 8 {
		return mp4HeaderUnknown
	}
	for off+8 <= int64(len(head)) {
		size := int64(binary.BigEndian.Uint32(head[off : off+4]))
		typ := string(head[off+4 : off+8])
		switch {
		case typ == "moov":
			return mp4HeaderFirst
		case typ == "mdat":
			return mp4HeaderLast
		}
		if size < 8 {
			return mp4HeaderUnknown
		}
		off += size
	}
	return mp4HeaderUnknown
}

// errNoMoov reports a file whose header could not be found at its end.
var errNoMoov = errors.New("pure: no moov box at the end of the file")

// tailWindow is how much of the end of a file is read when looking for a header
// that was written last. The window has to cover the whole moov box, because the
// box's own header is the last part of it, so it starts small and grows.
const tailWindow = 256 << 10

// tailMoov finds the moov box of a file whose header sits at the end. The box is
// located by its type rather than by walking sizes, because the start of the
// walk is inside a box whose header is somewhere earlier in the file and so is
// not known.
func tailMoov(rs io.ReadSeeker, size int64) ([]byte, error) {
	for window := int64(tailWindow); ; window *= 4 {
		if window > size {
			window = size
		}
		start := size - window
		if _, err := rs.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
		buf := make([]byte, window)
		if _, err := io.ReadFull(rs, buf); err != nil {
			return nil, err
		}
		for at := 0; at+8 <= len(buf); at++ {
			if string(buf[at+4:at+8]) != "moov" {
				continue
			}
			body, ok := moovBodyAt(rs, start+int64(at), buf[at:], size)
			if ok {
				return body, nil
			}
		}
		if window >= size || window >= maxHeader {
			return nil, errNoMoov
		}
	}
}

// moovBodyAt reads the body of a moov box whose header starts at the given file
// offset. The candidate is rejected unless its declared size lands inside the
// file, because the four letters can just as easily appear inside media data.
func moovBodyAt(rs io.ReadSeeker, headerAt int64, head []byte, size int64) ([]byte, bool) {
	if len(head) < 8 {
		return nil, false
	}
	boxSize := int64(binary.BigEndian.Uint32(head[:4]))
	header := int64(8)
	if boxSize == 1 {
		if len(head) < 16 {
			return nil, false
		}
		boxSize = int64(binary.BigEndian.Uint64(head[8:16]))
		header = 16
	}
	if boxSize < header || boxSize-header > maxHeader {
		return nil, false
	}
	body := boxSize - header
	if headerAt+boxSize > size {
		return nil, false
	}
	out := make([]byte, body)
	if _, err := rs.Seek(headerAt+header, io.SeekStart); err != nil {
		return nil, false
	}
	if _, err := io.ReadFull(rs, out); err != nil {
		return nil, false
	}
	return out, true
}

// openMP4Last reads an MP4 whose header sits after its media, which is what a
// file written for download looks like, and what FFmpeg writes by default. The
// header is found from the tail and the sample tables are read out of it; the
// reader is then moved back to the first sample, where the rest of the walk is
// forward like any other file.
func openMP4Last(rs io.ReadSeeker) (*mp4Audio, error) {
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	moov, err := tailMoov(rs, size)
	if err != nil {
		return nil, err
	}
	m, err := newMP4Audio(rs, 0, moov)
	if err != nil {
		return nil, err
	}
	// Everything before the header is media, so the first sample is the lowest
	// offset in the table and the walk runs forward from there.
	if _, err := rs.Seek(m.offsets[0], io.SeekStart); err != nil {
		return nil, err
	}
	m.consumed = m.offsets[0]
	return m, nil
}
