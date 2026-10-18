package pure

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// WebM is a subset of Matroska, and Matroska stores Opus packets exactly as the
// encoder produced them: the container adds framing and timestamps but never
// re-frames the codec payload. That is what makes a stream needing no processing
// forwardable without being decoded at all, so this reader is written to hand
// back whole packets rather than samples.
//
// Only the parts of EBML that reach an Opus track are understood. Anything else
// is skipped by its declared size, which is how the format is meant to be read.

// Elements this reader looks for. IDs keep the length marker that tells them
// apart, which is why they are not round numbers.
const (
	ebmlID      = 0x1A45DFA3
	segmentID   = 0x18538067
	infoID      = 0x1549A966
	tracksID    = 0x1654AE6B
	clusterID   = 0x1F43B675
	cuesID      = 0x1C53BB6B
	seekHeadID  = 0x114D9B74
	tagsID      = 0x1254C367
	chaptersID  = 0x1043A770
	attachID    = 0x1941A469
	timecodeID  = 0x2AD7B1
	timestampID = 0xE7

	simpleBlockID = 0xA3
	blockGroupID  = 0xA0
	blockID       = 0xA1

	trackEntryID   = 0xAE
	trackNumberID  = 0xD7
	trackTypeID    = 0x83
	codecIDFieldID = 0x86
	codecPrivateID = 0x63A2
	codecDelayID   = 0x56AA
	audioID        = 0xE1
	channelsID     = 0x9F

	// defaultTimecodeScale is one millisecond, which is what nearly every file
	// uses and what the format assumes when the element is absent.
	defaultTimecodeScale = 1000000
)

// maxElement bounds an element that is read into memory. A TrackEntry and an
// Info are both tiny; a block holds one packet.
const maxElement = 4 << 20

var errBadEBML = errors.New("pure: malformed EBML")

// webmTrack describes the Opus track a file carries.
type webmTrack struct {
	number   uint64
	channels int
	// preSkip is how many samples at the start carry no audio, which the
	// container records as CodecDelay.
	preSkip int
}

// webm reads an Opus track out of a WebM stream.
type webm struct {
	r     io.Reader
	track *webmTrack
	// scale converts a timestamp unit to nanoseconds.
	scale int64
	// clusterTime is the timestamp of the cluster being read, in scale units.
	clusterTime int64

	queue   [][]byte
	queueTS []int64
	done    bool
}

// openWebM reads forward until it has found the Opus track, leaving the reader
// positioned to continue into the clusters.
func openWebM(r io.Reader) (*webm, *webmTrack, error) {
	id, size, unknown, err := readElement(r)
	if err != nil {
		return nil, nil, err
	}
	if id != ebmlID || unknown {
		return nil, nil, fmt.Errorf("pure: not an EBML header")
	}
	if err := discard(r, int64(size)); err != nil {
		return nil, nil, err
	}
	id, size, unknown, err = readElement(r)
	if err != nil {
		return nil, nil, err
	}
	if id != segmentID {
		return nil, nil, fmt.Errorf("pure: no segment after the EBML header")
	}
	w := &webm{r: r, scale: defaultTimecodeScale}
	if !unknown {
		// A live stream can declare a segment far larger than what is served,
		// so this only ever narrows the reader, never widens it.
		w.r = io.LimitReader(r, int64(size))
	}
	// Walk the segment's leading elements until the track is known. Clusters
	// then continue from wherever that left the reader.
	for w.track == nil {
		id, size, unknown, err = readElement(w.r)
		if err != nil {
			if err == io.EOF {
				return nil, nil, fmt.Errorf("pure: no Opus track")
			}
			return nil, nil, err
		}
		if unknown && isLevelOne(id) {
			// An unknown sized cluster ends where the next level one element
			// begins, which is only detectable from inside the cluster walk.
			return nil, nil, fmt.Errorf("pure: unknown sized cluster before the track list")
		}
		switch id {
		case infoID:
			body, err := readBody(w.r, size)
			if err != nil {
				return nil, nil, err
			}
			if v, ok := childUint(body, timecodeID); ok && v > 0 {
				w.scale = int64(v)
			}
		case tracksID:
			body, err := readBody(w.r, size)
			if err != nil {
				return nil, nil, err
			}
			w.track = parseTracks(body)
		default:
			if err := discard(w.r, int64(size)); err != nil {
				return nil, nil, err
			}
		}
	}
	return w, w.track, nil
}

// isLevelOne reports the elements that can only appear directly inside a
// segment. Seeing one means an unknown sized element has ended.
func isLevelOne(id uint64) bool {
	switch id {
	case clusterID, infoID, tracksID, cuesID, seekHeadID, tagsID, chaptersID, attachID:
		return true
	}
	return false
}

// next returns the next Opus packet and the millisecond it starts at.
func (w *webm) next() ([]byte, int64, error) {
	for {
		if len(w.queue) > 0 {
			pkt, ts := w.queue[0], w.queueTS[0]
			w.queue, w.queueTS = w.queue[1:], w.queueTS[1:]
			return pkt, ts, nil
		}
		if w.done {
			return nil, 0, io.EOF
		}
		if err := w.step(); err != nil {
			return nil, 0, err
		}
	}
}

// step reads one element and reacts to it.
func (w *webm) step() error {
	id, size, unknown, err := readElement(w.r)
	if err != nil {
		if err == io.EOF {
			w.done = true
			return nil
		}
		return err
	}
	if unknown {
		// Only a cluster is ever written this way, and it runs until the next
		// level one element, which the loop below notices on its own.
		if id != clusterID {
			return errBadEBML
		}
		w.clusterTime = 0
		return nil
	}
	switch id {
	case clusterID:
		w.clusterTime = 0
		// The cluster's children follow; the size is not needed to walk them.
		return nil
	case timestampID:
		body, err := readBody(w.r, size)
		if err != nil {
			return err
		}
		w.clusterTime = int64(beUint(body))
	case simpleBlockID:
		body, err := readBody(w.r, size)
		if err != nil {
			return err
		}
		return w.addBlock(body)
	case blockGroupID:
		body, err := readBody(w.r, size)
		if err != nil {
			return err
		}
		if block := child(body, blockID); block != nil {
			return w.addBlock(block)
		}
	default:
		return discard(w.r, int64(size))
	}
	return nil
}

// addBlock records the audio packets a block carries, ignoring any other track.
func (w *webm) addBlock(data []byte) error {
	number, relative, frames, err := parseBlock(data)
	if err != nil {
		return err
	}
	if number != w.track.number {
		return nil
	}
	// The block's timestamp is relative to its cluster, and both are in units of
	// the timecode scale, which is milliseconds unless the file says otherwise.
	ms := (w.clusterTime + int64(relative)) * w.scale / 1000000
	for _, f := range frames {
		w.queue = append(w.queue, f)
		w.queueTS = append(w.queueTS, ms)
	}
	return nil
}

// readElement reads an element ID and its size.
func readElement(r io.Reader) (id, size uint64, unknown bool, err error) {
	id, _, err = readVintRaw(r)
	if err != nil {
		return 0, 0, false, err
	}
	size, n, err := readVintRaw(r)
	if err != nil {
		return 0, 0, false, err
	}
	if n < 1 || n > 8 {
		return 0, 0, false, errBadEBML
	}
	// The length marker is in the first byte, which is bit 7n of the value once
	// the bytes have been assembled. Clearing it leaves the value; every value
	// bit set means the size was left unknown, which a live stream does for its
	// clusters.
	size &^= uint64(1) << uint(7*n)
	unknown = size == (uint64(1)<<uint(7*n))-1
	return id, size, unknown, nil
}

// readVintRaw reads a variable length integer with its length marker intact.
func readVintRaw(r io.Reader) (uint64, int, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, 0, err
	}
	b := first[0]
	if b == 0 {
		return 0, 0, errBadEBML
	}
	n := 1
	for mask := byte(0x80); b&mask == 0; mask >>= 1 {
		n++
	}
	value := uint64(b)
	if n > 1 {
		rest := make([]byte, n-1)
		if _, err := io.ReadFull(r, rest); err != nil {
			return 0, 0, err
		}
		for _, c := range rest {
			value = value<<8 | uint64(c)
		}
	}
	return value, n, nil
}

// readID reads an element ID from a slice. IDs keep their length marker, which
// is the only thing that tells 0xAE from 0x2E, so it must not be cleared.
func readID(b []byte) (uint64, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || n > len(b) {
		return 0, 0, false
	}
	value := uint64(b[0])
	for i := 1; i < n; i++ {
		value = value<<8 | uint64(b[i])
	}
	return value, n, true
}

// readVint reads a variable length integer with the length marker stripped.
func readVint(b []byte) (uint64, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || n > len(b) {
		return 0, 0, false
	}
	value := uint64(b[0]) &^ (uint64(1) << uint(8-n))
	for i := 1; i < n; i++ {
		value = value<<8 | uint64(b[i])
	}
	return value, n, true
}

// readSignedVint reads the signed form EBML lacing uses for its deltas, where
// the bit below the length marker carries the sign.
func readSignedVint(b []byte) (int64, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || n > len(b) {
		return 0, 0, false
	}
	marker := uint64(1) << uint(8-n)
	value := uint64(b[0]) &^ marker &^ (marker >> 1)
	for i := 1; i < n; i++ {
		value = value<<8 | uint64(b[i])
	}
	out := int64(value)
	if b[0]&byte(marker>>1) != 0 {
		// Sign extend from the width of the value.
		out -= int64(uint64(1) << uint(7*n-1))
	}
	return out, n, true
}

// readBody reads a whole element, refusing anything unreasonably large.
func readBody(r io.Reader, size uint64) ([]byte, error) {
	if size > maxElement {
		return nil, fmt.Errorf("pure: element of %d bytes", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func discard(r io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, r, n)
	return err
}

// child returns the first child element with the wanted ID.
func child(b []byte, want uint64) []byte {
	for len(b) > 0 {
		id, n, ok := readID(b)
		if !ok {
			return nil
		}
		size, sn, ok := readVint(b[n:])
		if !ok {
			return nil
		}
		start := n + sn
		if uint64(start)+size > uint64(len(b)) {
			return nil
		}
		if id == want {
			return b[start : uint64(start)+size]
		}
		b = b[uint64(start)+size:]
	}
	return nil
}

func childUint(b []byte, want uint64) (uint64, bool) {
	v := child(b, want)
	if v == nil {
		return 0, false
	}
	return beUint(v), true
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// parseTracks finds the Opus audio track among the track entries.
func parseTracks(b []byte) *webmTrack {
	for len(b) > 0 {
		id, n, ok := readID(b)
		if !ok {
			return nil
		}
		size, sn, ok := readVint(b[n:])
		if !ok {
			return nil
		}
		start := n + sn
		if uint64(start)+size > uint64(len(b)) {
			return nil
		}
		if id == trackEntryID {
			if t := parseTrackEntry(b[start : uint64(start)+size]); t != nil {
				return t
			}
		}
		b = b[uint64(start)+size:]
	}
	return nil
}

func parseTrackEntry(b []byte) *webmTrack {
	kind, _ := childUint(b, trackTypeID)
	if kind != 2 { // 2 is audio
		return nil
	}
	codec := child(b, codecIDFieldID)
	if string(codec) != "A_OPUS" {
		return nil
	}
	number, ok := childUint(b, trackNumberID)
	if !ok {
		return nil
	}
	t := &webmTrack{number: number, channels: 2}
	if audio := child(b, audioID); audio != nil {
		if ch, ok := childUint(audio, channelsID); ok && ch > 0 && ch <= 2 {
			t.channels = int(ch)
		}
	}
	// CodecDelay is the encoder's priming in nanoseconds, and OpusHead carries
	// the same figure in samples. Either is enough; the container's is the one
	// a muxer is expected to keep correct.
	if delay, ok := childUint(b, codecDelayID); ok && delay > 0 {
		t.preSkip = int(delay * 48000 / 1000000000)
	} else if head := child(b, codecPrivateID); len(head) >= 12 && string(head[:8]) == "OpusHead" {
		t.preSkip = int(binary.LittleEndian.Uint16(head[10:12]))
	}
	if t.preSkip < 0 || t.preSkip > 48000 {
		t.preSkip = 0
	}
	return t
}

// parseBlock splits a block into the packets it carries. The block opens with
// the track number, a timestamp relative to the cluster and a flags byte, after
// which the frames are laid out according to the lacing bits.
func parseBlock(b []byte) (uint64, int16, [][]byte, error) {
	number, n, ok := readVint(b)
	if !ok || len(b) < n+3 {
		return 0, 0, nil, errBadEBML
	}
	relative := int16(binary.BigEndian.Uint16(b[n : n+2]))
	flags := b[n+2]
	body := b[n+3:]
	switch lacing := flags & 0x06; lacing {
	case 0x00:
		if len(body) == 0 {
			return number, relative, nil, nil
		}
		return number, relative, [][]byte{body}, nil
	case 0x02:
		frames, err := xiphLace(body)
		return number, relative, frames, err
	case 0x04:
		count := int(body[0]) + 1
		rest := body[1:]
		if count < 1 || len(rest)%count != 0 {
			return number, relative, nil, errBadEBML
		}
		size := len(rest) / count
		frames := make([][]byte, 0, count)
		for i := 0; i < count; i++ {
			frames = append(frames, rest[i*size:(i+1)*size])
		}
		return number, relative, frames, nil
	case 0x06:
		frames, err := ebmlLace(body)
		return number, relative, frames, err
	}
	return number, relative, [][]byte{body}, nil
}

// xiphLace reads the frame count and the sizes before the last frame.
func xiphLace(body []byte) ([][]byte, error) {
	if len(body) < 1 {
		return nil, errBadEBML
	}
	count := int(body[0]) + 1
	at := 1
	sizes := make([]int, 0, count)
	for i := 0; i < count-1; i++ {
		size := 0
		for {
			if at >= len(body) {
				return nil, errBadEBML
			}
			v := body[at]
			at++
			size += int(v)
			if v != 255 {
				break
			}
		}
		sizes = append(sizes, size)
	}
	return sliceFrames(body, at, sizes)
}

// ebmlLace reads the frame count, the first size, then signed deltas.
func ebmlLace(body []byte) ([][]byte, error) {
	if len(body) < 1 {
		return nil, errBadEBML
	}
	count := int(body[0]) + 1
	at := 1
	first, n, ok := readVint(body[at:])
	if !ok {
		return nil, errBadEBML
	}
	at += n
	sizes := []int{int(first)}
	for i := 1; i < count-1; i++ {
		delta, n, ok := readSignedVint(body[at:])
		if !ok {
			return nil, errBadEBML
		}
		at += n
		sizes = append(sizes, sizes[i-1]+int(delta))
	}
	return sliceFrames(body, at, sizes)
}

// sliceFrames cuts the declared sizes out of the payload and gives the last
// frame whatever is left.
func sliceFrames(body []byte, at int, sizes []int) ([][]byte, error) {
	frames := make([][]byte, 0, len(sizes)+1)
	for _, size := range sizes {
		if size < 0 || at+size > len(body) {
			return nil, errBadEBML
		}
		frames = append(frames, body[at:at+size])
		at += size
	}
	if at < len(body) {
		frames = append(frames, body[at:])
	}
	return frames, nil
}

// looksWebM reports the EBML magic that opens every WebM and Matroska file.
func looksWebM(head []byte) bool {
	return len(head) >= 4 && binary.BigEndian.Uint32(head[:4]) == ebmlID
}

// webmOpus reports a WebM or Matroska stream carrying Opus. The codec string is
// always within the first few hundred bytes, so the peek sees it.
func webmOpus(head []byte) bool {
	return looksWebM(head) && bytes.Contains(head, []byte("A_OPUS"))
}

// opusPacketSamples reports how many samples at 48 kHz one Opus packet carries.
// The duration is in the top five bits of the first byte, and the low two bits
// say how many frames are packed after it.
//
// The passthrough needs this because the player paces at a fixed 20 ms: a packet
// that is not exactly one such frame cannot be forwarded without decoding it.
func opusPacketSamples(pkt []byte) int {
	if len(pkt) < 1 {
		return 0
	}
	toc := pkt[0]
	var per int
	switch config := toc >> 3; {
	case config < 12: // SILK, narrow to wide band
		per = [...]int{480, 960, 1920, 2880}[config&3]
	case config < 16: // hybrid, super wide to full band
		per = [...]int{480, 960}[config&1]
	default: // CELT
		per = [...]int{120, 240, 480, 960}[config&3]
	}
	frames := 1
	switch toc & 0x03 {
	case 1, 2:
		frames = 2
	case 3:
		if len(pkt) < 2 {
			return 0
		}
		frames = int(pkt[1] & 0x3F)
	}
	return per * frames
}
