package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestTrackJSONWireShape pins the names a client reads. A rename or a missing
// null field is invisible to a Go round trip but breaks every client that binds
// track info.
func TestTrackJSONWireShape(t *testing.T) {
	uri := "https://example.com/watch"
	track, err := Encode(TrackInfo{
		Identifier: "id",
		IsSeekable: true,
		Author:     "Artist",
		Length:     1000,
		Title:      "Song",
		URI:        &uri,
		SourceName: "youtube",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	b, err := json.Marshal(track)
	if err != nil {
		t.Fatalf("marshal track: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal track: %v", err)
	}
	if len(raw) != 4 {
		t.Fatalf("track has %d fields, want 4: %s", len(raw), b)
	}
	for _, key := range []string{"encoded", "info", "pluginInfo", "userData"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("track is missing %q: %s", key, b)
		}
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(raw["info"], &info); err != nil {
		t.Fatalf("unmarshal info: %s", raw["info"])
	}
	want := []string{"identifier", "isSeekable", "author", "length", "isStream", "position", "title", "uri", "artworkUrl", "isrc", "sourceName"}
	if len(info) != len(want) {
		t.Fatalf("info has %d fields, want %d: %s", len(info), len(want), raw["info"])
	}
	for _, key := range want {
		if _, ok := info[key]; !ok {
			t.Fatalf("info is missing %q: %s", key, raw["info"])
		}
	}
	// Absent optional values stay in the payload as null; a client binding them
	// to nullable strings tells "no artwork" from "field not sent".
	if got := string(info["artworkUrl"]); got != "null" {
		t.Fatalf("absent artworkUrl is %s, want null", got)
	}
}

// TestParseInt covers the coercions a Jackson-backed Lavalink accepts, so the
// same request bodies keep working here.
func TestParseInt(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "integer literal", in: "42", want: 42},
		{name: "string holding an integer", in: `"42"`, want: 42},
		{name: "string padded with spaces", in: `" 42 "`, want: 42},
		{name: "float without a fraction", in: "42.0", want: 42},
		{name: "negative string", in: `"-7"`, want: -7},
		{name: "float with a fraction", in: "42.5", wantErr: true},
		{name: "null", in: "null", wantErr: true},
		{name: "empty string", in: `""`, wantErr: true},
		{name: "boolean", in: "true", wantErr: true},
		{name: "not a number", in: `"abc"`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseInt([]byte(c.in))
			if c.wantErr {
				if err == nil {
					t.Fatalf("accepted %s as %d", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected %s: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("%s parsed as %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestPlayerUpdateAcceptsCoercedNumbers pins the reason Int64 exists: a client
// that sends "position": "0" must not fail the whole update.
func TestPlayerUpdateAcceptsCoercedNumbers(t *testing.T) {
	var update PlayerUpdate
	if err := json.Unmarshal([]byte(`{"position":"1500","volume":80.0,"paused":true}`), &update); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if update.Position == nil || int64(*update.Position) != 1500 {
		t.Fatalf("position: %v", update.Position)
	}
	if update.Volume == nil || int64(*update.Volume) != 80 {
		t.Fatalf("volume: %v", update.Volume)
	}
	if update.Paused == nil || !*update.Paused {
		t.Fatalf("paused: %v", update.Paused)
	}

	var fractional PlayerUpdate
	if err := json.Unmarshal([]byte(`{"position":1.5}`), &fractional); err == nil {
		t.Fatalf("accepted a fractional position as %v", fractional.Position)
	}

	// An omitted position stays nil so the player can tell "leave it alone" from
	// "seek to zero".
	var omitted PlayerUpdate
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatalf("unmarshal empty update: %v", err)
	}
	if omitted.Position != nil {
		t.Fatalf("omitted position became %v", *omitted.Position)
	}
}

// TestTrackUpdateDistinguishesNullFromOmitted keeps the stop-versus-ignore
// distinction Lavalink gives encoded: null stops the track, while a missing
// encoded field means the update is about something else.
func TestTrackUpdateDistinguishesNullFromOmitted(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "explicit null", body: `{"encoded":null}`, want: "null"},
		{name: "omitted", body: `{}`, want: ""},
		{name: "encoded value", body: `{"encoded":"abc"}`, want: `"abc"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var update TrackUpdate
			if err := json.Unmarshal([]byte(c.body), &update); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := string(update.Encoded); got != c.want {
				t.Fatalf("encoded is %q, want %q", got, c.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	if got := String(""); got != nil {
		t.Fatalf("empty string became %q", *got)
	}
	if got := String("value"); got == nil || *got != "value" {
		t.Fatalf("value string: %v", got)
	}
}

// TestCloneCopiesDeeply pins what Clone is for: a caller edits its copy without
// changing the track the node published or has queued elsewhere.
func TestCloneCopiesDeeply(t *testing.T) {
	uri := "https://example.com/watch"
	original, err := Encode(TrackInfo{
		Identifier: "id",
		IsSeekable: true,
		Author:     "Artist",
		Length:     1000,
		Title:      "Song",
		URI:        &uri,
		SourceName: "youtube",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	original.PluginInfo["plugin"] = "value"
	original.UserData["requester"] = "user"

	clone := Clone(original)
	if clone == nil {
		t.Fatal("Clone returned nil for a track")
	}
	if got, want := infoJSON(t, clone.Info), infoJSON(t, original.Info); got != want {
		t.Fatalf("clone info: got %s, want %s", got, want)
	}
	if clone.Encoded != original.Encoded {
		t.Fatalf("clone encoded track: got %q", clone.Encoded)
	}
	if clone.Info.URI == original.Info.URI {
		t.Fatal("clone shares the uri pointer, so editing it would edit the original")
	}

	clone.Info.Title = "Changed"
	*clone.Info.URI = "https://example.com/other"
	clone.PluginInfo["plugin"] = "changed"
	clone.UserData["extra"] = true
	if original.Info.Title != "Song" {
		t.Fatalf("editing the clone changed the title to %q", original.Info.Title)
	}
	if original.Info.URI == nil || *original.Info.URI != uri {
		t.Fatalf("editing the clone changed the uri to %v", original.Info.URI)
	}
	if original.PluginInfo["plugin"] != "value" {
		t.Fatalf("editing the clone changed pluginInfo to %v", original.PluginInfo["plugin"])
	}
	if _, ok := original.UserData["extra"]; ok {
		t.Fatal("editing the clone added a key to the original userData")
	}

	if Clone(nil) != nil {
		t.Fatal("Clone(nil) is not nil")
	}
}

// TestLoadError pins the error result a client receives, including the source
// name that identifies where the failure came from.
func TestLoadError(t *testing.T) {
	result := LoadError(errors.New("no matching track"))
	if result.LoadType != "error" {
		t.Fatalf("loadType is %q", result.LoadType)
	}
	exception, ok := result.Data.(Exception)
	if !ok {
		t.Fatalf("data is %T, want Exception", result.Data)
	}
	if exception.Message != "no matching track" {
		t.Fatalf("message is %q", exception.Message)
	}
	if exception.Severity != "common" || exception.Cause != "golalink.source" {
		t.Fatalf("severity=%q cause=%q", exception.Severity, exception.Cause)
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal load error: %v", err)
	}
	if !strings.Contains(string(b), `"loadType":"error"`) || !strings.Contains(string(b), `"severity":"common"`) {
		t.Fatalf("load error marshalled without its identifying fields: %s", b)
	}
}
