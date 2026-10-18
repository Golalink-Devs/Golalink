// Package pure is an in-process audio pipeline: it decodes a container to PCM,
// resamples it to the 48 kHz stereo Discord expects, applies gain and encodes
// Opus packets. It exists so playback does not need FFmpeg for the formats it
// covers.
package pure

import "math"

// OutRate is the only sample rate the pipeline produces. Discord voice is Opus
// at 48 kHz, so everything upstream is converted to it.
const OutRate = 48000

// Channels is the only channel count the pipeline produces.
const Channels = 2

// Resampler converts interleaved stereo 16-bit PCM from one rate to 48 kHz with
// a polyphase windowed-sinc filter.
//
// The ratios a source can present are all rational (44100/48000 reduces to
// 147/160), so the filter is built as one row of coefficients per output phase,
// which makes the position exact integer arithmetic rather than an accumulator
// that drifts.
//
// A Resampler belongs to one stream: it carries the input history that the next
// call needs. It is not safe for concurrent use.
type Resampler struct {
	// l and m are the ratio: output n reads input position n*m/l. l is also the
	// number of output phases.
	l, m int
	// halfTaps is the filter radius in input frames on each side.
	halfTaps int
	taps     int
	coeffs   []float32 // l rows of taps coefficients
	// acc is the output index times m, so the input index is acc/l and the phase
	// is acc%l. Integer, so it never drifts.
	acc int
	// in holds buffered interleaved input, with origin giving the stream index of
	// in[0]. appended is the stream index just past the buffered data.
	in       []int16
	origin   int
	appended int
	// identity is set when the source already runs at 48 kHz, where the filter
	// would be a no-op.
	identity bool
	// zeros is the run of silence prepended so the first outputs have the full
	// filter footprint available.
	primed bool
}

// NewResampler builds a resampler from inRate to 48 kHz.
func NewResampler(inRate int) *Resampler {
	if inRate == OutRate {
		return &Resampler{identity: true}
	}
	g := gcd(inRate, OutRate)
	return newRatioResampler(OutRate/g, inRate/g)
}

// NewRatioResampler builds a resampler whose input runs at a fractional rate,
// which is what a pitch shift is: output frame n reads input frame n*m/l. The
// ratio is rounded to the closest fraction the phase table can hold.
func NewRatioResampler(l, m int) *Resampler {
	if l <= 0 || m <= 0 || l == m {
		return &Resampler{identity: true}
	}
	g := gcd(l, m)
	return newRatioResampler(l/g, m/g)
}

func newRatioResampler(l, m int) *Resampler {
	// A ratio with a zero side would leave the phase accumulator stuck, so it is
	// refused here rather than looping for ever in Process.
	if l <= 0 || m <= 0 {
		return &Resampler{identity: true}
	}
	ratio := float64(m) / float64(l)
	// The cutoff sits at whichever Nyquist is lower: when the input is consumed
	// faster than it is produced the filter has to remove what would otherwise
	// alias down. The radius grows as the cutoff falls, because a narrower
	// passband needs a longer filter to keep the same transition width.
	cutoff := math.Min(1, 1/ratio)
	halfTaps := int(math.Ceil(32 / cutoff))
	if halfTaps < 32 {
		halfTaps = 32
	}
	if halfTaps > 128 {
		halfTaps = 128
	}
	taps := 2 * halfTaps
	const beta = 8.0
	norm := besselI0(beta)
	coeffs := make([]float32, l*taps)
	for p := 0; p < l; p++ {
		frac := float64(p) / float64(l)
		row := coeffs[p*taps : (p+1)*taps]
		var sum float64
		for k := 0; k < taps; k++ {
			// Distance in input frames from the interpolated point.
			x := float64(k-halfTaps+1) - frac
			h := cutoff * sinc(cutoff*x)
			if r := x / float64(halfTaps); math.Abs(r) <= 1 {
				h *= besselI0(beta*math.Sqrt(1-r*r)) / norm
			} else {
				h = 0
			}
			row[k] = float32(h)
			sum += h
		}
		// Normalising each phase to unity DC gain keeps a constant input at a
		// constant level whatever the rounding in the design.
		if sum != 0 {
			for k := range row {
				row[k] /= float32(sum)
			}
		}
	}
	return &Resampler{l: l, m: m, halfTaps: halfTaps, taps: taps, coeffs: coeffs}
}

// Latency reports the filter delay in output frames. The pipeline reports it so
// a seek lands on the right sample.
func (r *Resampler) Latency() int {
	if r.identity {
		return 0
	}
	return r.halfTaps * r.l / r.m
}

// Process appends the resampled form of src to dst and returns it. Calls are
// streaming: successive chunks of one stream go to one Resampler in order.
func (r *Resampler) Process(src []int16, dst []int16) []int16 {
	if r.identity {
		return append(dst, src...)
	}
	r.append(src)
	for {
		inIdx := r.acc / r.l
		if inIdx+r.halfTaps >= r.appended {
			break // the filter footprint runs past what has arrived
		}
		base := inIdx - r.halfTaps + 1 - r.origin
		row := r.coeffs[(r.acc%r.l)*r.taps : (r.acc%r.l+1)*r.taps]
		for ch := 0; ch < Channels; ch++ {
			var sum float32
			for k := 0; k < r.taps; k++ {
				sum += row[k] * float32(r.in[(base+k)*Channels+ch])
			}
			dst = append(dst, round16(sum))
		}
		r.acc += r.m
	}
	r.trim()
	return dst
}

// append copies src into the buffer, priming it with the silence the filter
// needs before the first real frame.
func (r *Resampler) append(src []int16) {
	if !r.primed {
		r.primed = true
		r.origin = -(r.halfTaps - 1)
		r.in = make([]int16, (r.halfTaps-1)*Channels)
		r.appended = 0
	}
	r.in = append(r.in, src...)
	r.appended += len(src) / Channels
}

// trim drops the frames the filter will never read again.
func (r *Resampler) trim() {
	keep := r.acc/r.l - r.halfTaps + 1
	if drop := keep - r.origin; drop > 0 {
		n := drop * Channels
		if n >= len(r.in) {
			r.in = r.in[:0]
		} else {
			r.in = append(r.in[:0], r.in[n:]...)
		}
		r.origin = keep
	}
}

// round16 rounds to the nearest integer and saturates. A Go float-to-int
// conversion truncates towards zero, which would add a small DC offset.
func round16(v float32) int16 {
	f := float64(v)
	n := int32(f + math.Copysign(0.5, f))
	if n > math.MaxInt16 {
		return math.MaxInt16
	}
	if n < math.MinInt16 {
		return math.MinInt16
	}
	return int16(n)
}

// sinc is the normalised sinc, sin(pi x)/(pi x).
func sinc(x float64) float64 {
	if math.Abs(x) < 1e-9 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// besselI0 is the zeroth-order modified Bessel function, which the Kaiser window
// is built from. The series converges quickly for the beta values used here.
func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	half := x / 2
	for k := 1; k < 40; k++ {
		term *= (half / float64(k)) * (half / float64(k))
		sum += term
		if term < 1e-12*sum {
			break
		}
	}
	return sum
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}
