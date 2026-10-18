package pure

// When a track has more than two channels, FFmpeg folds it to stereo before the
// encoder sees it, and the in-process path has to fold it the same way. If it
// did not, a surround track would change both its loudness and its balance
// depending on which engine happened to play it.
//
// These are the coefficients FFmpeg's graph uses, measured from its own output
// rather than taken from its source. Every channel goes to one side, except the
// centre and the back centre, which go to both, and the low frequency channel,
// which is dropped.
//
// The mix is not scaled down to keep it from clipping, even though the front and
// centre coefficients sum well past one. FFmpeg's behaviour depends on the shape
// of the filter graph: a bare conversion to two channels scales the mix by
// one over its sum, but the graph Golalink builds, where aformat is followed by
// the volume node, does not. The graph is what the in-process engine has to
// agree with, so the unscaled coefficients are the ones used here.
const (
	// sideMix is M_SQRT1_2, what a centre or a surround channel contributes.
	sideMix = 0.7071067811865476
	// backCentreMix is a back centre channel: FFmpeg mixes it at the centre
	// level through the surround level.
	backCentreMix = 0.5
)

// surroundMix holds the mix for each channel count from three up, in the channel
// order the containers use, which is front left, front right, front centre, low
// frequency, then the surrounds. It is indexed by the count rather than by a
// layout name because that order is fixed by the container, so the count is all
// the pipeline is ever told.
//
// Mono and stereo are not here: a mono source is upmixed by a single fixed gain
// and a stereo one is copied, and both predate this table.
var surroundMix = map[int][][2]float64{
	3: {{1, 0}, {0, 1}, {sideMix, sideMix}},
	4: {{1, 0}, {0, 1}, {sideMix, 0}, {0, sideMix}},
	5: {{1, 0}, {0, 1}, {sideMix, sideMix}, {sideMix, 0}, {0, sideMix}},
	6: {{1, 0}, {0, 1}, {sideMix, sideMix}, {0, 0}, {sideMix, 0}, {0, sideMix}},
	7: {{1, 0}, {0, 1}, {sideMix, sideMix}, {0, 0},
		{backCentreMix, backCentreMix}, {sideMix, 0}, {0, sideMix}},
	8: {{1, 0}, {0, 1}, {sideMix, sideMix}, {0, 0},
		{sideMix, 0}, {0, sideMix}, {sideMix, 0}, {0, sideMix}},
}

// maxChannels is the widest layout the mix covers. FLAC stops at eight and no
// other container the pipeline reads goes past it, so anything wider is left to
// FFmpeg rather than guessed at.
const maxChannels = 8

// downmix folds interleaved PCM of three or more channels into interleaved
// stereo. The caller has already handled mono and stereo.
func downmix(src []int16, channels int, dst []int16) []int16 {
	mix, ok := surroundMix[channels]
	if !ok {
		return dst
	}
	for i := 0; i+channels <= len(src); i += channels {
		var left, right float64
		for c, m := range mix {
			v := float64(src[i+c])
			left += v * m[0]
			right += v * m[1]
		}
		dst = append(dst, clamp16(left), clamp16(right))
	}
	return dst
}
