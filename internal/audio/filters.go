package audio

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"golalink/internal/protocol"
)

// DisabledFilters reports which filters in a request the operator turned off in
// the config, in the order the reference implementation reports them. A filter
// that is absent from the config stays enabled, and unknown filter names are
// left to Build, which rejects them.
func DisabledFilters(filters protocol.Object, configured map[string]bool) []string {
	var names []string
	for _, name := range FilterNames {
		if _, present := filters[name]; !present {
			continue
		}
		if enabled, ok := configured[name]; ok && !enabled {
			names = append(names, name)
		}
	}
	return names
}

var FilterNames = []string{"volume", "equalizer", "karaoke", "timescale", "tremolo", "vibrato", "rotation", "distortion", "channelMix", "lowPass"}

func number(v float64) string { return strconv.FormatFloat(v, 'f', 8, 64) }

type field struct {
	name          string
	def, min, max float64
}

func object(raw any, fields ...field) (map[string]float64, error) {
	b, e := json.Marshal(raw)
	if e != nil {
		return nil, e
	}
	var m map[string]float64
	if e = json.Unmarshal(b, &m); e != nil || m == nil {
		return nil, fmt.Errorf("filter must be an object with numeric values")
	}
	out := map[string]float64{}
	for _, f := range fields {
		v, ok := m[f.name]
		if !ok {
			v = f.def
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < f.min || v > f.max {
			return nil, fmt.Errorf("%s must be in [%g, %g]", f.name, f.min, f.max)
		}
		out[f.name] = v
		delete(m, f.name)
	}
	if len(m) > 0 {
		return nil, fmt.Errorf("unknown filter parameter")
	}
	return out, nil
}

// equalizerFrequencies are the fifteen bands, lowest first. The protocol names a
// band by its index into this list.
var equalizerFrequencies = []int{25, 40, 63, 100, 160, 250, 400, 630, 1000, 1600, 2500, 4000, 6300, 10000, 16000}

// FilterSet is a validated request. The FFmpeg engine renders it as a filter
// graph and the in-process engine applies the same settings to samples, so both
// read one parse and cannot drift apart.
type FilterSet struct {
	// Gain is the global volume and the volume filter combined.
	Gain     float64
	Bands    []EQBand
	Karaoke  *Karaoke
	Time     *Timescale
	Tremolo  *Modulation
	Vibrato  *Modulation
	Rotation *Rotation
	Distort  *Distortion
	Mix      *ChannelMix
	LowPass  *LowPass
}

// EQBand is one peaking band, already converted from the protocol's gain scale.
type EQBand struct {
	Frequency float64
	Decibels  float64
}

// Karaoke subtracts the centre channel, scaled by Gain.
type Karaoke struct {
	FilterBand  float64
	FilterWidth float64
	// Gain is negative: the centre channel is taken away from the mix.
	Gain float64
}

// Timescale changes the speed and the pitch of a track independently.
type Timescale struct {
	// Speed changes tempo and keeps the pitch.
	Speed float64
	// Pitch changes pitch and keeps the tempo.
	Pitch float64
	// Rate changes both.
	Rate float64
}

// Modulation is the shared shape of tremolo and vibrato.
type Modulation struct {
	Frequency float64
	Depth     float64
}

// Rotation pans the two channels against each other.
type Rotation struct{ Hertz float64 }

// Distortion is the waveshaper the protocol exposes.
type Distortion struct {
	SinOffset, SinScale float64
	CosOffset, CosScale float64
	TanOffset, TanScale float64
	Offset, Scale       float64
}

// ChannelMix is the four-way stereo mixdown matrix.
type ChannelMix struct {
	LeftToLeft, LeftToRight, RightToLeft, RightToRight float64
}

// LowPass is a one pole low pass, held as the frequency it was derived from.
type LowPass struct {
	// Smoothing is the protocol value; Frequency is what the filter uses.
	Smoothing float64
	Frequency float64
}

// Tempo is the rate the track advances at, which the player uses to pace frames.
func (f *FilterSet) Tempo() float64 {
	if f.Time == nil {
		return 1
	}
	return f.Time.Speed * f.Time.Rate
}

// ChangesAnything reports whether the time scaling was asked for at all. A
// request that leaves all three controls at one asks for nothing, and running it
// would only add the delay the stretch introduces.
func (t *Timescale) ChangesAnything() bool {
	return t.Speed != 1 || t.Pitch != 1 || t.Rate != 1
}

// Active lists the filters a request actually asked for, in the order the graph
// applies them. A filter whose settings amount to doing nothing is left out, so
// the list is what has to be honoured rather than what was mentioned. The
// in-process pipeline diffs this against what it can apply, which is what stops
// it from silently playing a request as if it had not been made.
func (f *FilterSet) Active() []string {
	var names []string
	if len(f.Bands) > 0 {
		names = append(names, "equalizer")
	}
	if f.Karaoke != nil {
		names = append(names, "karaoke")
	}
	if f.Time != nil && f.Time.ChangesAnything() {
		names = append(names, "timescale")
	}
	if f.Tremolo != nil {
		names = append(names, "tremolo")
	}
	if f.Vibrato != nil {
		names = append(names, "vibrato")
	}
	if f.Rotation != nil {
		names = append(names, "rotation")
	}
	if f.Distort != nil {
		names = append(names, "distortion")
	}
	if f.Mix != nil {
		names = append(names, "channelMix")
	}
	if f.LowPass != nil {
		names = append(names, "lowPass")
	}
	return names
}

// Parse validates a filter request without building anything. Both engines go
// through it, so a request one accepts is a request the other accepts.
func Parse(filters protocol.Object, volume int) (*FilterSet, error) {
	if volume < 0 || volume > 1000 {
		return nil, fmt.Errorf("volume must be 0..1000")
	}
	for name := range filters {
		known := false
		for _, n := range FilterNames {
			known = known || name == n
		}
		if name == "pluginFilters" {
			v, ok := filters[name].(map[string]any)
			if !ok || len(v) > 0 {
				return nil, fmt.Errorf("plugin filters are not installed")
			}
			continue
		}
		if !known {
			return nil, fmt.Errorf("unsupported filter: %s", name)
		}
	}

	set := &FilterSet{Gain: float64(volume) / 100}
	if raw, ok := filters["volume"]; ok {
		v, ok := raw.(float64)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 5 {
			return nil, fmt.Errorf("filter volume must be 0..5")
		}
		set.Gain *= v
	}
	if raw, ok := filters["equalizer"]; ok {
		b, _ := json.Marshal(raw)
		var bands []struct {
			Band int      `json:"band"`
			Gain *float64 `json:"gain"`
		}
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.DisallowUnknownFields()
		if e := d.Decode(&bands); e != nil || len(bands) > 15 {
			return nil, fmt.Errorf("invalid equalizer")
		}
		seen := map[int]bool{}
		for _, b := range bands {
			// An omitted gain is the protocol default of 1.0, not a flat band:
			// the reference implementation declares `@EncodeDefault var gain = 1.0f`,
			// and a missing field therefore deserialises to 1.0.
			gain := 1.0
			if b.Gain != nil {
				gain = *b.Gain
			}
			if b.Band < 0 || b.Band > 14 || gain < -0.25 || gain > 1 || seen[b.Band] {
				return nil, fmt.Errorf("invalid or duplicate equalizer band")
			}
			seen[b.Band] = true
			set.Bands = append(set.Bands, EQBand{
				Frequency: float64(equalizerFrequencies[b.Band]),
				Decibels:  20 * math.Log10(math.Max(.0001, 1+4*gain)),
			})
		}
	}
	if raw, ok := filters["karaoke"]; ok {
		v, e := object(raw, field{"level", 1, 0, 1}, field{"monoLevel", 1, 0, 1}, field{"filterBand", 220, 1, 24000}, field{"filterWidth", 100, 1, 24000})
		if e != nil {
			return nil, e
		}
		set.Karaoke = &Karaoke{
			FilterBand:  v["filterBand"],
			FilterWidth: v["filterWidth"],
			Gain:        -v["level"] * v["monoLevel"],
		}
	}
	if raw, ok := filters["timescale"]; ok {
		v, e := object(raw, field{"speed", 1, .05, 5}, field{"pitch", 1, .05, 5}, field{"rate", 1, .05, 5})
		if e != nil {
			return nil, e
		}
		set.Time = &Timescale{Speed: v["speed"], Pitch: v["pitch"], Rate: v["rate"]}
	}
	for _, name := range []string{"tremolo", "vibrato"} {
		raw, ok := filters[name]
		if !ok {
			continue
		}
		v, e := object(raw, field{"frequency", 2, .1, 20}, field{"depth", .5, 0, 1})
		if e != nil {
			return nil, e
		}
		m := &Modulation{Frequency: v["frequency"], Depth: v["depth"]}
		if name == "tremolo" {
			set.Tremolo = m
		} else {
			set.Vibrato = m
		}
	}
	if raw, ok := filters["rotation"]; ok {
		v, e := object(raw, field{"rotationHz", .2, -20, 20})
		if e != nil {
			return nil, e
		}
		if v["rotationHz"] != 0 {
			set.Rotation = &Rotation{Hertz: v["rotationHz"]}
		}
	}
	if raw, ok := filters["distortion"]; ok {
		v, e := object(raw, field{"sinOffset", 0, -10, 10}, field{"sinScale", 1, -10, 10}, field{"cosOffset", 0, -10, 10}, field{"cosScale", 1, -10, 10}, field{"tanOffset", 0, -10, 10}, field{"tanScale", 1, -10, 10}, field{"offset", 0, -10, 10}, field{"scale", 1, -10, 10})
		if e != nil {
			return nil, e
		}
		set.Distort = &Distortion{
			SinOffset: v["sinOffset"], SinScale: v["sinScale"],
			CosOffset: v["cosOffset"], CosScale: v["cosScale"],
			TanOffset: v["tanOffset"], TanScale: v["tanScale"],
			Offset: v["offset"], Scale: v["scale"],
		}
	}
	if raw, ok := filters["channelMix"]; ok {
		v, e := object(raw, field{"leftToLeft", 1, 0, 1}, field{"leftToRight", 0, 0, 1}, field{"rightToLeft", 0, 0, 1}, field{"rightToRight", 1, 0, 1})
		if e != nil {
			return nil, e
		}
		set.Mix = &ChannelMix{
			LeftToLeft: v["leftToLeft"], LeftToRight: v["leftToRight"],
			RightToLeft: v["rightToLeft"], RightToRight: v["rightToRight"],
		}
	}
	if raw, ok := filters["lowPass"]; ok {
		v, e := object(raw, field{"smoothing", 20, 1.0001, 1000})
		if e != nil {
			return nil, e
		}
		set.LowPass = &LowPass{Smoothing: v["smoothing"], Frequency: 48000 / (2 * math.Pi * v["smoothing"])}
	}
	return set, nil
}

// Build returns a validated FFmpeg filter graph. No user-supplied text enters it.
func Build(filters protocol.Object, volume int) (string, float64, error) {
	set, err := Parse(filters, volume)
	if err != nil {
		return "", 0, err
	}
	return set.Render(), set.Tempo(), nil
}

// tempoChain renders one tempo change as the sequence of atempo stages FFmpeg
// needs, because a single stage only covers 0.5 to 2.
func tempoChain(v float64) []string {
	var out []string
	for v < 0.5 {
		out = append(out, "atempo=0.5")
		v /= 0.5
	}
	for v > 2 {
		out = append(out, "atempo=2")
		v /= 2
	}
	return append(out, "atempo="+number(v))
}

// Render writes the graph. The node order is fixed, so the same request always
// produces the same graph whatever order the fields arrived in.
func (f *FilterSet) Render() string {
	parts := []string{"aformat=sample_rates=48000:channel_layouts=stereo", "volume=" + number(f.Gain)}
	for _, b := range f.Bands {
		parts = append(parts, fmt.Sprintf("equalizer=f=%d:t=o:w=0.666667:g=%s", int(b.Frequency), number(b.Decibels)))
	}
	if k := f.Karaoke; k != nil {
		parts = append(parts, "asplit=2[kdry][kmid];[kmid]pan=mono|c0=0.5*c0+0.5*c1,bandpass=f="+number(k.FilterBand)+":t=h:w="+number(k.FilterWidth)+",volume="+number(k.Gain)+",pan=stereo|c0=c0|c1=c0[kcut];[kdry][kcut]amix=inputs=2:normalize=0:duration=first")
	}
	if t := f.Time; t != nil {
		parts = append(parts, "asetrate="+number(48000*t.Pitch*t.Rate), "aresample=48000")
		parts = append(parts, tempoChain(t.Speed/t.Pitch)...)
	}
	if m := f.Tremolo; m != nil {
		parts = append(parts, "tremolo=f="+number(m.Frequency)+":d="+number(m.Depth))
	}
	if m := f.Vibrato; m != nil {
		parts = append(parts, "vibrato=f="+number(m.Frequency)+":d="+number(m.Depth))
	}
	if r := f.Rotation; r != nil {
		hz := number(r.Hertz)
		parts = append(parts, "aeval=exprs='val(0)*(0.5+0.5*cos(2*PI*"+hz+"*t))|val(1)*(0.5-0.5*cos(2*PI*"+hz+"*t))'")
	}
	if d := f.Distort; d != nil {
		expr := func(ch string) string {
			x := "val(" + ch + ")"
			return "clip(" + number(d.Scale) + "*(sin(" + x + "*" + number(d.SinScale) + "+" + number(d.SinOffset) + ")*cos(" + x + "*" + number(d.CosScale) + "+" + number(d.CosOffset) + ")*tan(" + x + "*" + number(d.TanScale) + "+" + number(d.TanOffset) + "))+" + number(d.Offset) + ",-1,1)"
		}
		parts = append(parts, "aeval=exprs='"+expr("0")+"|"+expr("1")+"'")
	}
	if m := f.Mix; m != nil {
		parts = append(parts, "pan=stereo|c0="+number(m.LeftToLeft)+"*c0+"+number(m.RightToLeft)+"*c1|c1="+number(m.LeftToRight)+"*c0+"+number(m.RightToRight)+"*c1")
	}
	if l := f.LowPass; l != nil {
		parts = append(parts, "lowpass=f="+number(l.Frequency)+":p=1")
	}
	return strings.Join(parts, ",")
}
