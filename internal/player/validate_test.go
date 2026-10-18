package player

import (
	"encoding/json"
	"strings"
	"testing"

	"golalink/internal/protocol"
)

// TestValidateVolume pins the accepted range. A rejected volume must fail the
// whole request rather than be clamped, because a client that asked for 1001%
// should hear about the mistake instead of hearing something else.
func TestValidateVolume(t *testing.T) {
	cases := []struct {
		name   string
		volume *protocol.Int64
		ok     bool
	}{
		{name: "absent", ok: true},
		{name: "silent", volume: i64(0), ok: true},
		{name: "loudest allowed", volume: i64(1000), ok: true},
		{name: "above the ceiling", volume: i64(1001)},
		{name: "negative", volume: i64(-1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(protocol.PlayerUpdate{Volume: c.volume})
			switch {
			case c.ok && err != nil:
				t.Fatalf("unexpected rejection: %v", err)
			case !c.ok && (err == nil || !strings.Contains(err.Error(), "volume")):
				t.Fatalf("expected a volume error, got %v", err)
			}
		})
	}
}

// TestValidatePositionAndEndTime covers the two clock fields. Position is a seek
// offset that cannot be negative; endTime may be a JSON number, the string form
// Jackson-based clients send, or null to clear it, but nothing else.
func TestValidatePositionAndEndTime(t *testing.T) {
	cases := []struct {
		name   string
		update protocol.PlayerUpdate
		ok     bool
		want   string
	}{
		{name: "no position", ok: true},
		{name: "position zero", update: protocol.PlayerUpdate{Position: i64(0)}, ok: true},
		{name: "position ahead", update: protocol.PlayerUpdate{Position: i64(60_000)}, ok: true},
		{name: "position negative", update: protocol.PlayerUpdate{Position: i64(-1)}, want: "nonnegative"},

		{name: "no end time", ok: true},
		{name: "end time null", update: protocol.PlayerUpdate{EndTime: json.RawMessage("null")}, ok: true},
		{name: "end time number", update: protocol.PlayerUpdate{EndTime: json.RawMessage("90000")}, ok: true},
		{name: "end time string", update: protocol.PlayerUpdate{EndTime: json.RawMessage(`"90000"`)}, ok: true},
		{name: "end time zero", update: protocol.PlayerUpdate{EndTime: json.RawMessage("0")}, want: "positive"},
		{name: "end time negative", update: protocol.PlayerUpdate{EndTime: json.RawMessage("-1000")}, want: "positive"},
		{name: "end time not a number", update: protocol.PlayerUpdate{EndTime: json.RawMessage(`"soon"`)}, want: "positive"},
		{name: "end time fractional", update: protocol.PlayerUpdate{EndTime: json.RawMessage("1.5")}, want: "positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.update)
			switch {
			case c.ok && err != nil:
				t.Fatalf("unexpected rejection: %v", err)
			case !c.ok && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("expected an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}

// TestValidateTrackSelection pins which combinations name exactly one track. The
// player can start or stop a track but never guesses between two, and the
// deprecated top-level fields cannot quietly compete with the nested one.
func TestValidateTrackSelection(t *testing.T) {
	valid := seekableTrack(t, "abc", "Title", 120_000)
	raw := encodedRaw(t, valid)
	cases := []struct {
		name   string
		update protocol.PlayerUpdate
		ok     bool
		want   string
	}{
		{name: "encoded track", update: protocol.PlayerUpdate{EncodedTrack: raw}, ok: true},
		{name: "identifier", update: protocol.PlayerUpdate{Identifier: str("abc")}, ok: true},
		{name: "clearing with null", update: stopUpdate(), ok: true},
		{name: "encoded and identifier", update: protocol.PlayerUpdate{EncodedTrack: raw, Identifier: str("abc")}, want: "mutually exclusive"},
		{name: "nested track plus deprecated identifier", update: protocol.PlayerUpdate{Track: &protocol.TrackUpdate{Identifier: str("abc")}, Identifier: str("abc")}, want: "deprecated"},
		{name: "nested encoded plus nested identifier", update: protocol.PlayerUpdate{Track: &protocol.TrackUpdate{Encoded: raw, Identifier: str("abc")}}, want: "mutually exclusive"},
		// An empty identifier names nothing, so it is rejected before the loader
		// could treat it as a lookup.
		{name: "empty identifier", update: protocol.PlayerUpdate{Identifier: str("")}, want: "must not be empty"},
		{name: "encoded is not a string", update: protocol.PlayerUpdate{Track: &protocol.TrackUpdate{Encoded: json.RawMessage("123")}}, want: "string or null"},
		{name: "encoded is not a track", update: protocol.PlayerUpdate{EncodedTrack: json.RawMessage(`"not-a-track"`)}, want: "invalid encoded track"},
		{name: "encoded is empty", update: protocol.PlayerUpdate{EncodedTrack: json.RawMessage(`""`)}, want: "invalid encoded track"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.update)
			switch {
			case c.ok && err != nil:
				t.Fatalf("unexpected rejection: %v", err)
			case !c.ok && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("expected an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}

// TestValidateFiltersAndVoice checks that a request is rejected before it can
// change anything: an unsupported filter or a voice state that would send the
// node somewhere other than a Discord voice host fails validation, not playback.
func TestValidateFiltersAndVoice(t *testing.T) {
	timescale := protocol.Object{"timescale": map[string]any{"speed": 1.5}}
	validVoice := voiceState(channelA)
	noToken := voiceState(channelA)
	noToken.Token = ""
	foreignHost := voiceState(channelA)
	foreignHost.Endpoint = "example.com:443"

	cases := []struct {
		name   string
		update protocol.PlayerUpdate
		ok     bool
		want   string
	}{
		{name: "no filters", ok: true},
		{name: "valid filter", update: protocol.PlayerUpdate{Filters: &timescale}, ok: true},
		{name: "unknown filter", update: protocol.PlayerUpdate{Filters: &protocol.Object{"bogus": 1.0}}, want: "unsupported filter"},
		{name: "filter volume out of range", update: protocol.PlayerUpdate{Filters: &protocol.Object{"volume": 9.0}}, want: "0..5"},
		{name: "invalid equalizer band", update: protocol.PlayerUpdate{Filters: &protocol.Object{"equalizer": []map[string]any{{"band": 20, "gain": 0.1}}}}, want: "equalizer"},

		{name: "valid voice", update: protocol.PlayerUpdate{Voice: &validVoice}, ok: true},
		{name: "voice without a token", update: protocol.PlayerUpdate{Voice: &noToken}, want: "voice requires"},
		{name: "voice off a non-Discord host", update: protocol.PlayerUpdate{Voice: &foreignHost}, want: "Discord"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.update)
			switch {
			case c.ok && err != nil:
				t.Fatalf("unexpected rejection: %v", err)
			case !c.ok && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("expected an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}
