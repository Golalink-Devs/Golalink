package pure

import (
	"math"

	"golalink/internal/audio"
)

// The filter chain applies a validated request to the samples between the
// resampler and the encoder, in the order the FFmpeg graph lists them:
//
//	volume, equalizer, karaoke, timescale, tremolo, vibrato,
//	rotation, distortion, channelMix, lowPass
//
// The volume is folded into a single gain rather than a stage. Every filter here
// works on interleaved stereo at 48 kHz, in the integer sample units the rest of
// the pipeline carries; FFmpeg's own integer path does the same.

// Chain is the filter settings applied to a stream of samples.
type Chain struct {
	stages []stage
}

// stage transforms interleaved stereo in place and returns the samples to pass
// on. Only a stage that changes the length can return a different slice.
type stage interface {
	process(buf []int16) []int16
}

// flusher is a stage that holds samples back, so the end of the stream has to
// ask it for whatever is left.
type flusher interface{ flush() []int16 }

// errNoInProcess names a filter the in-process pipeline cannot apply yet, so the
// dispatcher hands the track to FFmpeg instead. It is returned rather than
// ignored, because silently dropping a filter would play the wrong audio.
type errNoInProcess struct{ name string }

func (e errNoInProcess) Error() string {
	return "pure: the " + e.name + " filter is not available in process"
}

// handled lists the filters the chain can apply. The volume is not a stage: the
// stream multiplies the samples before the chain runs, so it counts as handled
// without one.
var handled = map[string]bool{
	"volume": true, "equalizer": true, "karaoke": true, "timescale": true,
	"tremolo": true, "vibrato": true, "rotation": true, "distortion": true,
	"channelMix": true, "lowPass": true,
}

// newChain builds the stages for a request. A request with nothing to do yields
// no chain at all, which keeps the common path free of the work.
func newChain(set *audio.FilterSet) (*Chain, error) {
	// Every filter the parse accepted has to end up in a stage, or the request
	// would play as if it had never been made. The diff is what makes that a
	// refusal rather than a silent difference in what a listener hears.
	for _, name := range set.Active() {
		if !handled[name] {
			return nil, errNoInProcess{name}
		}
	}
	c := &Chain{}
	if len(set.Bands) > 0 {
		c.stages = append(c.stages, newEqualizer(set.Bands))
	}
	if set.Karaoke != nil {
		c.stages = append(c.stages, newKaraoke(*set.Karaoke))
	}
	if set.Time != nil && set.Time.ChangesAnything() {
		c.stages = append(c.stages, newTimescale(*set.Time))
	}
	if set.Tremolo != nil {
		c.stages = append(c.stages, newTremolo(*set.Tremolo))
	}
	if set.Vibrato != nil {
		c.stages = append(c.stages, newVibrato(*set.Vibrato))
	}
	if set.Rotation != nil {
		c.stages = append(c.stages, newRotation(*set.Rotation))
	}
	if set.Distort != nil {
		c.stages = append(c.stages, newDistortion(*set.Distort))
	}
	if set.Mix != nil {
		c.stages = append(c.stages, newChannelMix(*set.Mix))
	}
	if set.LowPass != nil {
		c.stages = append(c.stages, newLowPass(*set.LowPass))
	}
	if len(c.stages) == 0 {
		return nil, nil
	}
	return c, nil
}

// process runs a block through every stage.
// process runs a block through the chain. Every stage works on the block it is
// handed and never writes past its length, so a caller that passes a window
// onto a larger buffer cannot have what follows it trampled. A stage that
// changes how many samples there are keeps a buffer of its own, so the result
// is only good until the next call.
func (c *Chain) process(buf []int16) []int16 {
	for _, s := range c.stages {
		buf = s.process(buf)
	}
	return buf
}

// flush collects what the stages were still holding when the stream ended. Only
// one stage ever holds anything back, and whatever it releases still has to pass
// through the stages after it, so they are run over the tail rather than the
// tail being appended to their own.
func (c *Chain) flush() []int16 {
	for i, s := range c.stages {
		f, ok := s.(flusher)
		if !ok {
			continue
		}
		tail := f.flush()
		for _, rest := range c.stages[i+1:] {
			tail = rest.process(tail)
		}
		return tail
	}
	return nil
}

// sampleRate is the rate every filter in the graph is specified against.
const sampleRate = 48000

// clamp16 converts a sample held at the pipeline's own scale back to the sample
// format, saturating rather than wrapping.
func clamp16(v float64) int16 {
	if v >= 32767 {
		return 32767
	}
	if v <= -32768 {
		return -32768
	}
	return int16(v + math.Copysign(0.5, v))
}

// clampNormalized is clamp16 for a value held in the -1..1 range the aeval
// expressions work in. The two scales are not interchangeable, so they get
// separate names rather than one function with an implicit unit.
func clampNormalized(v float64) int16 { return clamp16(v * 32768) }

// biquad is the second order section FFmpeg's filters are built from. The
// coefficients are stored normalised, so the recursion subtracts them.
type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

// run is FFmpeg's direct form I: y[n] = b.x - a.y, evaluated in that order.
func (b *biquad) run(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

const ln2 = 0.69314718055994530942

// newEqualizerBand is the two-pole peaking filter FFmpeg's `equalizer` builds
// with an octave width, which is how the graph always asks for it.
//
// The width is not turned into a Q: FFmpeg feeds the octave figure straight into
// alpha, which is subtly different from the textbook conversion and is the
// difference between matching its output and only sounding similar.
func newEqualizerBand(frequency, decibels, widthOctaves float64) biquad {
	w0 := 2 * math.Pi * frequency / sampleRate
	sin := math.Sin(w0)
	a := math.Pow(10, decibels/40)
	alpha := sin * math.Sinh(ln2/2*widthOctaves*w0/sin)
	a0 := 1 + alpha/a
	return biquad{
		b0: (1 + alpha*a) / a0,
		b1: (-2 * math.Cos(w0)) / a0,
		b2: (1 - alpha*a) / a0,
		a1: (-2 * math.Cos(w0)) / a0,
		a2: (1 - alpha/a) / a0,
	}
}

// equalizerWidth is the octave width the protocol fixes, `w=0.666667`.
const equalizerWidth = 0.666667

// equalizerStage runs one peaking filter per band per channel, cascaded in the
// order the request listed them.
type equalizerStage struct {
	bands []eqBand
}

type eqBand struct{ left, right biquad }

func newEqualizer(bands []audio.EQBand) *equalizerStage {
	e := &equalizerStage{bands: make([]eqBand, 0, len(bands))}
	for _, b := range bands {
		e.bands = append(e.bands, eqBand{
			left:  newEqualizerBand(b.Frequency, b.Decibels, equalizerWidth),
			right: newEqualizerBand(b.Frequency, b.Decibels, equalizerWidth),
		})
	}
	return e
}

func (e *equalizerStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		l, r := float64(buf[i]), float64(buf[i+1])
		for b := range e.bands {
			l = e.bands[b].left.run(l)
			r = e.bands[b].right.run(r)
		}
		buf[i], buf[i+1] = clamp16(l), clamp16(r)
	}
	return buf
}

// newBandpassHz is FFmpeg's bandpass with its width given in hertz, which is the
// form a constant-skirt bandpass with Q = centre/width reduces to.
func newBandpassHz(centre, width float64) biquad {
	w0 := 2 * math.Pi * centre / sampleRate
	alpha := math.Sin(w0) / (2 * centre / width)
	a0 := 1 + alpha
	return biquad{
		b0: alpha / a0,
		b2: -alpha / a0,
		a1: (-2 * math.Cos(w0)) / a0,
		a2: (1 - alpha) / a0,
	}
}

// karaokeStage takes the centre channel away from the mix. The graph splits the
// signal, collapses it to mono, keeps only a band of it, scales that band and
// subtracts it from the original. The band is mono, so one filter serves both
// channels.
type karaokeStage struct {
	band biquad
	gain float64
}

func newKaraoke(k audio.Karaoke) *karaokeStage {
	return &karaokeStage{band: newBandpassHz(k.FilterBand, k.FilterWidth), gain: k.Gain}
}

func (k *karaokeStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		dryL, dryR := float64(buf[i]), float64(buf[i+1])
		// pan=mono|c0=0.5*c0+0.5*c1
		mid := k.band.run(0.5*dryL + 0.5*dryR)
		// volume then pan back to both channels, then a plain sum with the dry
		// signal, because the mix runs with normalization off.
		cut := k.gain * mid
		buf[i], buf[i+1] = clamp16(dryL+cut), clamp16(dryR+cut)
	}
	return buf
}

// biquadStage runs one second order section on each channel. A section is
// defined for a single channel, so stereo needs one set of state per side.
type biquadStage struct{ left, right biquad }

// newBiquadStage gives each channel its own copy of the section, so the two do
// not share the state that makes a filter a filter.
func newBiquadStage(b biquad) *biquadStage { return &biquadStage{left: b, right: b} }

func (s *biquadStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		buf[i] = clamp16(s.left.run(float64(buf[i])))
		buf[i+1] = clamp16(s.right.run(float64(buf[i+1])))
	}
	return buf
}

// newLowPass is FFmpeg's one-pole low pass. The width and Q it computes are
// unused in this form, so only the smoothing matters.
func newLowPass(l audio.LowPass) *biquadStage {
	x := math.Exp(-2 * math.Pi * l.Frequency / sampleRate)
	return newBiquadStage(biquad{b0: 1 - x, a1: -x})
}

// rotationStage pans the two channels against each other, which is the aeval
// expression the graph builds. The phase is absolute time, so it is advanced by
// one sample each time rather than restarted per block.
type rotationStage struct {
	step  float64
	phase float64
}

func newRotation(r audio.Rotation) *rotationStage {
	return &rotationStage{step: 2 * math.Pi * r.Hertz / sampleRate}
}

func (r *rotationStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		c := math.Cos(r.phase)
		buf[i] = clamp16(float64(buf[i]) * (0.5 + 0.5*c))
		buf[i+1] = clamp16(float64(buf[i+1]) * (0.5 - 0.5*c))
		r.phase += r.step
	}
	return buf
}

// distortionStage is the waveshaper the aeval expression describes. The signal
// is normalised to unit scale first, because that is the range the expression
// clips back into.
type distortionStage struct{ d audio.Distortion }

func newDistortion(d audio.Distortion) *distortionStage { return &distortionStage{d: d} }

func (s *distortionStage) process(buf []int16) []int16 {
	for i, v := range buf {
		x := float64(v) / 32768
		y := s.d.Scale *
			math.Sin(x*s.d.SinScale+s.d.SinOffset) *
			math.Cos(x*s.d.CosScale+s.d.CosOffset) *
			math.Tan(x*s.d.TanScale+s.d.TanOffset)
		y += s.d.Offset
		if y > 1 {
			y = 1
		} else if y < -1 {
			y = -1
		}
		buf[i] = clampNormalized(y)
	}
	return buf
}

// channelMixStage is the four-way stereo matrix.
type channelMixStage struct{ m audio.ChannelMix }

func newChannelMix(m audio.ChannelMix) *channelMixStage { return &channelMixStage{m: m} }

func (s *channelMixStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		l, r := float64(buf[i]), float64(buf[i+1])
		buf[i] = clamp16(s.m.LeftToLeft*l + s.m.RightToLeft*r)
		buf[i+1] = clamp16(s.m.LeftToRight*l + s.m.RightToRight*r)
	}
	return buf
}

// modulationTableSize is the rounded sample count one cycle of a modulator gets.
// FFmpeg rounds the length rather than carrying a fractional period, so a cycle
// closes a fraction short and the phase steps by a fixed amount. Reproducing the
// rounding is what keeps the two in step.
func modulationTableSize(frequency float64) int {
	return int(math.RoundToEven(sampleRate/frequency + 0.5))
}

// tremoloStage applies the amplitude envelope FFmpeg precomputes into a table.
// The envelope is a raised cosine, the same gain on both channels, and the index
// advances once per frame rather than once per sample.
type tremoloStage struct {
	table []float64
	index int
}

func newTremolo(m audio.Modulation) *tremoloStage {
	offset := 1 - m.Depth/2
	scale := 1 - math.Abs(offset)
	table := make([]float64, modulationTableSize(m.Frequency))
	for i := range table {
		env := m.Frequency * float64(i) / sampleRate
		env = math.Sin(2 * math.Pi * math.Mod(env+0.25, 1))
		table[i] = env*scale + offset
	}
	return &tremoloStage{table: table}
}

func (t *tremoloStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		g := t.table[t.index]
		buf[i] = clamp16(float64(buf[i]) * g)
		buf[i+1] = clamp16(float64(buf[i+1]) * g)
		if t.index++; t.index >= len(t.table) {
			t.index = 0
		}
	}
	return buf
}

// vibratoDelay is the length of the delay line FFmpeg allocates, which is the
// ceiling on how far the pitch can be swung.
const vibratoDelay = sampleRate * 5 / 1000

// vibratoStage delays the signal by an amount that moves up and down, which
// bends the pitch. The delay is read with linear interpolation between two
// neighbouring samples, so it can take a fractional value.
type vibratoStage struct {
	// lines holds one delay line per channel; the two must not share state.
	lines [2][]float64
	// delay is the precomputed swing, in samples, wrapped as it is read.
	delay []float64
	write int
	index int
	depth float64
}

func newVibrato(m audio.Modulation) *vibratoStage {
	// The table holds the delay in samples. It is built as a sine wave mapped
	// onto the range the delay line can address, starting a quarter turn in.
	size := modulationTableSize(m.Frequency)
	phaseOffset := uint32(3*math.Pi/2/math.Pi/2*float64(size) + 0.5)
	table := make([]float64, size)
	for i := range table {
		point := (uint32(i) + phaseOffset) % uint32(size)
		d := (math.Sin(float64(point)/float64(size)*2*math.Pi) + 1) / 2
		table[i] = d * float64(vibratoDelay-1)
	}
	return &vibratoStage{
		lines: [2][]float64{make([]float64, vibratoDelay), make([]float64, vibratoDelay)},
		delay: table,
		depth: m.Depth,
	}
}

func (v *vibratoStage) process(buf []int16) []int16 {
	for i := 0; i+1 < len(buf); i += 2 {
		integer, decimal := math.Modf(v.depth * v.delay[v.index])
		if v.index++; v.index >= len(v.delay) {
			v.index = 0
		}
		// The write cursor is one past the newest sample, so reading forward
		// from it reaches back into the line.
		first := v.write + int(integer)
		if first >= vibratoDelay {
			first -= vibratoDelay
		}
		second := first + 1
		if second >= vibratoDelay {
			second -= vibratoDelay
		}
		for c := 0; c < 2; c++ {
			line := v.lines[c]
			// The input has to be read before the output replaces it, because
			// the line stores what came in, not what went out.
			in := float64(buf[i+c])
			buf[i+c] = clamp16(line[first] + decimal*(line[second]-line[first]))
			line[v.write] = in
		}
		if v.write++; v.write >= vibratoDelay {
			v.write = 0
		}
	}
	return buf
}
