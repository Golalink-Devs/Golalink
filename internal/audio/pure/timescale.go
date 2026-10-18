package pure

import (
	"math"

	"golalink/internal/audio"
)

// The time stretch is a WSOLA: the signal is cut into overlapping windowed
// frames and the frames are laid back down at a different spacing. Moving them
// closer together shortens the sound, moving them apart lengthens it, and what
// keeps the pitch is that the frames themselves are untouched.
//
// The frames cannot simply be cut at fixed intervals, because the point where
// one ends and the next begins would be a discontinuity, which is heard as a
// click and a buzz. So each frame is taken from wherever it best matches what
// the previous frame was already saying, which is what the search below does.
//
// The sizes are the ones FFmpeg's atempo settles on for 48 kHz: a window of the
// power of two at or above a 24th of a second, hopped by half of itself and
// searched over the other half.
const (
	stretchWindow = 2048
	stretchHop    = stretchWindow / 2
	stretchSearch = stretchWindow / 2
)

// wsola changes the duration of a stream by a factor while leaving its pitch
// alone. A wsola belongs to one stream and is not safe for concurrent use.
type wsola struct {
	tempo float64
	hann  []float64

	// in holds the input history, origin gives the stream frame index of in[0]
	// and appended the index just past it.
	in       []int16
	origin   int
	appended int

	// acc collects the overlapping frames. Output before emitted is finished;
	// accOrigin is the stream frame index of acc[0], so the buffer can be
	// trimmed as it goes rather than growing with the whole track.
	acc       []float64
	accOrigin int
	emitted   int

	// frame is the next window to place, previous the input frame it was taken
	// from last time, and target the position the next one would naturally fall
	// on before the search moves it.
	frame    int
	previous int
	target   int
}

func newWSOLA(tempo float64) *wsola {
	hann := make([]float64, stretchWindow)
	for i := range hann {
		t := float64(i) / float64(stretchWindow-1)
		hann[i] = 0.5 * (1 - math.Cos(2*math.Pi*t))
	}
	return &wsola{tempo: tempo, hann: hann}
}

// read returns one input frame, treating anything outside what has arrived as
// silence. Reading past the end as silence is what gives the last frames
// something to be built from instead of cutting the tail short.
func (w *wsola) read(i int) (float64, float64) {
	at := (i - w.origin) * 2
	if at < 0 || at+1 >= len(w.in) {
		return 0, 0
	}
	return float64(w.in[at]), float64(w.in[at+1])
}

// correlate scores how well the frame at cand carries on from the frame at
// templateAt, over one overlap. It is the normalised correlation, so a quiet
// passage is not preferred merely for being quiet.
//
// The search calls this thousands of times per window, so the case where both
// frames lie inside what has arrived, which is all but the ends of the stream,
// is taken without the bounds checks the general read needs.
func (w *wsola) correlate(cand, templateAt int) float64 {
	cBase := (cand - w.origin) * 2
	tBase := (templateAt - w.origin) * 2
	span := stretchHop * 2
	if cBase >= 0 && tBase >= 0 && cBase+span <= len(w.in) && tBase+span <= len(w.in) {
		in := w.in
		cand, template := in[cBase:cBase+span], in[tBase:tBase+span]
		var dot, energy float64
		for i, b := range template {
			a := float64(cand[i])
			dot += a * float64(b)
			energy += a * a
		}
		if energy <= 1 {
			return 0
		}
		return dot / math.Sqrt(energy)
	}
	var dot, energy float64
	for i := 0; i < stretchHop; i++ {
		cl, cr := w.read(cand + i)
		tl, tr := w.read(templateAt + i)
		dot += cl*tl + cr*tr
		energy += cl*cl + cr*cr
	}
	if energy <= 1 {
		return 0
	}
	return dot / math.Sqrt(energy)
}

// stretchTarget is where the window at index k would sit before the search
// moves it. Placing them this far apart is what changes the duration: the
// spacing follows the tempo while the windows themselves do not change.
func (w *wsola) stretchTarget(k int) int {
	return int(math.Round(float64(k) * w.tempo * float64(stretchHop)))
}

// reach is the last input frame the window at index k can read: its position,
// plus the room the search may move it by, plus its own length.
func (w *wsola) reach(k int) int {
	return w.stretchTarget(k) + stretchSearch + stretchWindow
}

// step places the next window.
func (w *wsola) step() {
	k := w.frame
	w.target = w.stretchTarget(k)
	at := w.target
	if k > 0 {
		// What would carry on from the last window is read from just past its
		// start, and the window that best matches it joins without a seam.
		templateAt := w.previous + stretchHop
		best, bestScore := 0, math.Inf(-1)
		for d := -stretchSearch; d <= stretchSearch; d++ {
			if w.target+d < 0 {
				continue
			}
			if score := w.correlate(w.target+d, templateAt); score > bestScore {
				bestScore, best = score, d
			}
		}
		at = w.target + best
	}
	w.blend(at, k*stretchHop)
	w.previous = at
	w.frame++
}

// blend adds one Hann-windowed input frame into the output at the given
// position. Neighbouring frames overlap by half a window, where the two halves
// of the window sum to one, so the result needs no rescaling.
func (w *wsola) blend(inAt, outAt int) {
	// The accumulator is trimmed as it is handed out, so its length is measured
	// from accOrigin rather than from the start of the stream.
	if need := (outAt + stretchWindow - w.accOrigin) * 2; need > len(w.acc) {
		w.acc = append(w.acc, make([]float64, need-len(w.acc))...)
	}
	for i := 0; i < stretchWindow; i++ {
		if at := outAt + i - w.accOrigin; at < 0 {
			// Already handed out, so no later window can be adding to it.
			continue
		}
		l, r := w.read(inAt + i)
		g := w.hann[i]
		at := (outAt + i - w.accOrigin) * 2
		w.acc[at] += g * l
		w.acc[at+1] += g * r
	}
}

// drain hands out the output that no later window can still add to. A window
// placed at k*stretchHop is the first that can reach position k*stretchHop, so
// everything below the next window's position is finished.
func (w *wsola) drain(dst []int16) []int16 {
	limit := w.frame * stretchHop
	if max := w.accOrigin + len(w.acc)/2; limit > max {
		limit = max
	}
	for w.emitted < limit {
		at := (w.emitted - w.accOrigin) * 2
		dst = append(dst, clamp16(w.acc[at]), clamp16(w.acc[at+1]))
		w.emitted++
	}
	return dst
}

// reclaim drops the output that has already been handed out.
func (w *wsola) reclaim() {
	if w.emitted <= w.accOrigin {
		return
	}
	n := (w.emitted - w.accOrigin) * 2
	w.acc = append(w.acc[:0], w.acc[n:]...)
	w.accOrigin = w.emitted
}

// trim drops the input no future window can reach. A window only ever reads
// forward from its own position less the search room, and the templates it
// matches against sit later still, so this one's position is below every index
// the next window will want.
func (w *wsola) trim() {
	keep := w.target - stretchSearch
	if drop := keep - w.origin; drop > 0 {
		if drop*2 >= len(w.in) {
			w.in = w.in[:0]
		} else {
			w.in = append(w.in[:0], w.in[drop*2:]...)
		}
		w.origin = keep
	}
}

// push adds a block of input and returns whatever output it completed.
func (w *wsola) push(src []int16, dst []int16) []int16 {
	w.in = append(w.in, src...)
	w.appended += len(src) / 2
	// A window is read from wherever the search may move it to, so all of that
	// span has to have arrived before it can be placed. Placing it a frame short
	// would quietly build it out of silence, which only shows up once the stream
	// arrives in blocks rather than all at once.
	for w.frame*stretchHop < w.outputLen() && w.reach(w.frame) <= w.appended {
		w.step()
	}
	dst = w.drain(dst)
	w.reclaim()
	w.trim()
	return dst
}

// flush finishes the stream, building the last windows from silence so the tail
// keeps its length.
func (w *wsola) flush(dst []int16) []int16 {
	for w.frame*stretchHop < w.outputLen() {
		w.step()
	}
	return w.drain(dst)
}

// outputLen is how many frames the stretch is meant to produce in total.
func (w *wsola) outputLen() int {
	return int(math.Round(float64(w.appended) / w.tempo))
}

// timescaleStage changes the speed and the pitch of a stream. FFmpeg does this
// as two steps, and the order matters: a resample relabels the input so it plays
// at a different rate, which moves the pitch and the speed together, and then a
// stretch moves the speed on its own. Every combination of the three controls
// falls out of the two factors below.
type timescaleStage struct {
	rs *Resampler
	ws *wsola
	// buf holds the resampled input and out the stretched result. The stretch
	// changes how many samples there are, so its output needs a buffer of its
	// own: writing it back into the block it was handed would run past that
	// block's length and, when the caller's slice is a window onto something
	// larger, over whatever follows it.
	buf []int16
	out []int16
}

func newTimescale(t audio.Timescale) *timescaleStage {
	// asetrate=48000*pitch*rate followed by aresample=48000 is a resample by
	// that factor. The tempo that is left over, speed/pitch, is the stretch:
	// pitch and rate divide out, leaving a track that runs at speed*rate.
	l, m := ratioFraction(t.Pitch*t.Rate, ratioLimit)
	return &timescaleStage{
		rs: NewRatioResampler(l, m),
		ws: newWSOLA(t.Speed / t.Pitch),
	}
}

func (s *timescaleStage) process(buf []int16) []int16 {
	s.buf = s.rs.Process(buf, s.buf[:0])
	if len(s.buf) == 0 {
		return buf[:0]
	}
	s.out = s.ws.push(s.buf, s.out[:0])
	return s.out
}

// flush releases the tail the stretch is still holding. It comes back in a
// fresh buffer rather than the one process fills, so asking for the tail cannot
// disturb the last block the caller is still reading.
func (s *timescaleStage) flush() []int16 {
	return s.ws.flush(nil)
}

// ratioLimit bounds the denominator of the fraction a fractional rate is
// rounded to. The pitch controls are given to two decimal places, so anything
// past a few hundred phases is rounding noise, and each phase costs a row of
// filter coefficients.
const ratioLimit = 1024

// ratioFraction finds the fraction closest to ratio whose denominator is at
// most limit. It walks the Stern-Brocot tree, where every fraction is the
// mediant of the two that bracket it and the walk visits the best
// approximations in order of denominator, so the answer is the closest one the
// limit allows and the search is short.
//
// It returns the fraction as the numerator and denominator the resampler wants,
// which is the input frames read per output frame.
func ratioFraction(ratio float64, limit int) (l, m int) {
	if !(ratio > 0) || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		return 1, 1
	}
	// 0/1 and 1/0 bracket every positive ratio.
	loN, loD, hiN, hiD := 0, 1, 1, 0
	for {
		midN, midD := loN+hiN, loD+hiD
		if midD > limit {
			break
		}
		switch v := float64(midN) / float64(midD); {
		case v < ratio:
			loN, loD = midN, midD
		case v > ratio:
			hiN, hiD = midN, midD
		default:
			return midD, midN
		}
	}
	// The upper bound is still infinity if the walk never moved it, and the
	// lower one is still zero if it never moved that. Either way the other bound
	// is the answer.
	if hiD == 0 {
		return loD, loN
	}
	if loN == 0 {
		return hiD, hiN
	}
	if math.Abs(float64(loN)/float64(loD)-ratio) <= math.Abs(float64(hiN)/float64(hiD)-ratio) {
		return loD, loN
	}
	return hiD, hiN
}
