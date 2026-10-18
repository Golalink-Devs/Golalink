package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// These encoded values were assembled by hand from the three Lavaplayer layouts
// rather than by Encode, so they say what a client actually sends instead of
// what this package happens to produce.
const (
	// Version 1 predates the URI: only title, author, length, identifier,
	// stream flag, source name and position are on the wire.
	encodedV1 = "AAAALgADT2xkAAVCYW5kMQAAAAAAAAPoAANpZDkAAApzb3VuZGNsb3VkAAAAAAAAAAA="
	// Version 2 adds the URI.
	encodedV2 = "QAAAOgIAAlYyAARBdXRoAAAAAAAAE4gAA3ZpZAABAA5odHRwczovL2UuY28vdgAGbmF0aXZlAAAAAAAAAPo="
	// Version 3 adds artwork and ISRC; this one carries both and no source data.
	encodedV3 = "QAAAYQMABFRlc3QAAUEAAAAAAAK/IAAGYWJjMTIzAAEAFWh0dHBzOi8vZXhhbXBsZS5jb20veAEAGWh0dHBzOi8vZXhhbXBsZS5jb20vYS5wbmcAAAd5b3V0dWJlAAAAAAAABdw="
	// Version 3 again, but with Lavaplayer's source-specific bytes between the
	// source name and the final position.
	encodedV3Extension = "QAAANAMABlN0cmVhbQAFUmFkaW8AAAAAAAAAAAAEbGl2ZQEAAAAABGh0dHDerb7vAAAAAAAAEGg="
)

// infoJSON renders track info as JSON. Comparing that rather than the struct
// compares optional fields by the value they point at, and a mismatch shows both
// full sides instead of one field at a time.
func infoJSON(t *testing.T, info TrackInfo) string {
	t.Helper()
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal track info: %v", err)
	}
	return string(b)
}

// TestEncodeDecodeRoundTrip is the contract a Lavalink client depends on: the
// bytes written by Encode are read back by Decode into the same track info.
// IsSeekable is not on the wire; Decode derives it from the stream flag, which
// is why every case here sets it to what that derivation yields.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	uri := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	artwork := "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg"
	isrc := "GBARL9300135"
	cases := []struct {
		name string
		info TrackInfo
	}{
		{
			name: "every field",
			info: TrackInfo{
				Identifier: "dQw4w9WgXcQ",
				IsSeekable: true,
				Author:     "Rick Astley",
				Length:     213000,
				Position:   42000,
				Title:      "Never Gonna Give You Up",
				URI:        &uri,
				ArtworkURL: &artwork,
				ISRC:       &isrc,
				SourceName: "youtube",
			},
		},
		{
			name: "stream without optional fields",
			info: TrackInfo{
				Identifier: "live",
				Author:     "Radio",
				Title:      "Live",
				IsStream:   true,
				SourceName: "http",
				Position:   7,
			},
		},
		{
			name: "text outside the BMP and an embedded NUL",
			info: TrackInfo{
				Identifier: "x",
				IsSeekable: true,
				Author:     "YOASOBI",
				Length:     255000,
				Title:      "夜に駆ける 🎵\x00",
				SourceName: "youtube",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			track, err := Encode(c.info)
			if err != nil {
				t.Fatalf("encode %s: %v", c.name, err)
			}
			if got, want := infoJSON(t, track.Info), infoJSON(t, c.info); got != want {
				t.Fatalf("encode changed the info: got %s, want %s", got, want)
			}
			// A client unmarshalling pluginInfo or userData into a map rejects
			// null, so an encoded track must carry empty objects, not nil maps.
			if track.PluginInfo == nil || track.UserData == nil {
				t.Fatalf("encode left nil maps: pluginInfo=%v userData=%v", track.PluginInfo, track.UserData)
			}
			decoded, err := Decode(track.Encoded)
			if err != nil {
				t.Fatalf("decode %q: %v", track.Encoded, err)
			}
			if got, want := infoJSON(t, decoded.Info), infoJSON(t, c.info); got != want {
				t.Fatalf("round trip changed the info: got %s, want %s", got, want)
			}
			if decoded.Encoded != track.Encoded {
				t.Fatalf("decode rewrote the encoded track: got %q, want %q", decoded.Encoded, track.Encoded)
			}
		})
	}
}

// TestDecodeKnownVectors pins the layout of each format version, including the
// fields an older version does not carry. A refactor that quietly reorders or
// drops a field still round-trips with itself but fails against these.
func TestDecodeKnownVectors(t *testing.T) {
	uriV2 := "https://e.co/v"
	uriV3 := "https://example.com/x"
	artworkV3 := "https://example.com/a.png"
	cases := []struct {
		name    string
		encoded string
		want    TrackInfo
	}{
		{
			name:    "version 1 has no uri",
			encoded: encodedV1,
			want: TrackInfo{
				Identifier: "id9",
				IsSeekable: true,
				Author:     "Band1",
				Length:     1000,
				Title:      "Old",
				SourceName: "soundcloud",
			},
		},
		{
			name:    "version 2 adds the uri",
			encoded: encodedV2,
			want: TrackInfo{
				Identifier: "vid",
				IsSeekable: true,
				Author:     "Auth",
				Length:     5000,
				Position:   250,
				Title:      "V2",
				URI:        &uriV2,
				SourceName: "native",
			},
		},
		{
			name:    "version 3 adds artwork and isrc",
			encoded: encodedV3,
			want: TrackInfo{
				Identifier: "abc123",
				IsSeekable: true,
				Author:     "A",
				Length:     180000,
				Position:   1500,
				Title:      "Test",
				URI:        &uriV3,
				ArtworkURL: &artworkV3,
				SourceName: "youtube",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			track, err := Decode(c.encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got, want := infoJSON(t, track.Info), infoJSON(t, c.want); got != want {
				t.Fatalf("decoded info: got %s, want %s", got, want)
			}
			if track.Encoded != c.encoded {
				t.Fatalf("decode did not preserve the encoding: got %q", track.Encoded)
			}
		})
	}
}

// TestDecodeKeepsSourceExtension covers the bytes a source plugin appends before
// the position. They are not ours to interpret, but their presence must not
// shift the position or lose the original encoding, which is what a client
// echoes back when it later plays the track.
func TestDecodeKeepsSourceExtension(t *testing.T) {
	track, err := Decode(encodedV3Extension)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if track.Info.Title != "Stream" || track.Info.SourceName != "http" {
		t.Fatalf("decoded the wrong track: %#v", track.Info)
	}
	if !track.Info.IsStream || track.Info.IsSeekable {
		t.Fatalf("stream flags: isStream=%v isSeekable=%v", track.Info.IsStream, track.Info.IsSeekable)
	}
	if track.Info.Position != 4200 {
		t.Fatalf("position read from the wrong offset: got %d, want 4200", track.Info.Position)
	}
	if track.Encoded != encodedV3Extension {
		t.Fatalf("decode did not preserve the encoding: got %q", track.Encoded)
	}
}

// TestDecodeAcceptsUnpaddedBase64 matters because some clients strip the "="
// padding before sending a track back.
func TestDecodeAcceptsUnpaddedBase64(t *testing.T) {
	unpadded := strings.TrimRight(encodedV3, "=")
	track, err := Decode(unpadded)
	if err != nil {
		t.Fatalf("decode unpadded: %v", err)
	}
	if track.Info.Title != "Test" || track.Info.Position != 1500 {
		t.Fatalf("decoded the wrong track: %#v", track.Info)
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	// rewriteHeader keeps a shortened body and its length prefix consistent,
	// keeping the version flag so the case fails on the missing field rather than
	// on a stale header.
	rewriteHeader := func(b []byte) {
		header := uint32(len(b)-4) | 1<<30
		b[0] = byte(header >> 24)
		b[1] = byte(header >> 16)
		b[2] = byte(header >> 8)
		b[3] = byte(header)
	}
	mutated := func(edit func([]byte) []byte) string {
		b, err := base64.StdEncoding.DecodeString(encodedV3)
		if err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		return base64.StdEncoding.EncodeToString(edit(b))
	}

	cases := []struct {
		name    string
		encoded string
	}{
		{name: "empty", encoded: ""},
		{name: "not base64", encoded: "%%%"},
		{name: "shorter than a header", encoded: base64.StdEncoding.EncodeToString([]byte{0x40, 0, 0, 0})},
		{
			name: "header length does not match the body",
			encoded: mutated(func(b []byte) []byte {
				b[3] ^= 0x01
				return b
			}),
		},
		{
			name: "reserved header bit set",
			encoded: mutated(func(b []byte) []byte {
				b[0] |= 0x80
				return b
			}),
		},
		{
			name: "unknown version",
			encoded: mutated(func(b []byte) []byte {
				b[4] = 4
				return b
			}),
		},
		{
			name: "negative length",
			encoded: mutated(func(b []byte) []byte {
				b[14] |= 0x80
				return b
			}),
		},
		{
			name: "stream flag beyond a boolean",
			encoded: mutated(func(b []byte) []byte {
				b[30] = 2
				return b
			}),
		},
		{
			name: "presence flag beyond a boolean",
			encoded: mutated(func(b []byte) []byte {
				b[31] = 2
				return b
			}),
		},
		{
			name: "position missing",
			encoded: mutated(func(b []byte) []byte {
				b = b[:len(b)-8]
				rewriteHeader(b)
				return b
			}),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if track, err := Decode(c.encoded); err == nil {
				t.Fatalf("accepted malformed input as %#v", track)
			}
		})
	}
}

// TestDecodeRejectsOversizedInput keeps the size cap ahead of any allocation or
// base64 work, since the value arrives from a client.
func TestDecodeRejectsOversizedInput(t *testing.T) {
	if _, err := Decode(strings.Repeat("A", MaxEncodedTrack+1)); err == nil {
		t.Fatal("decoded a track longer than MaxEncodedTrack")
	}
}

// TestEncodeRejectsTextOverJavaUTFLimit pins the boundary of the two-byte length
// prefix. A title over it would otherwise be silently truncated into a track
// every client misreads.
func TestEncodeRejectsTextOverJavaUTFLimit(t *testing.T) {
	if _, err := Encode(TrackInfo{Title: strings.Repeat("a", 65536)}); err == nil {
		t.Fatal("encoded a title that does not fit a Java UTF length prefix")
	}
	track, err := Encode(TrackInfo{Title: strings.Repeat("a", 65535), SourceName: "youtube"})
	if err != nil {
		t.Fatalf("encode at the limit: %v", err)
	}
	decoded, err := Decode(track.Encoded)
	if err != nil {
		t.Fatalf("decode at the limit: %v", err)
	}
	if len(decoded.Info.Title) != 65535 {
		t.Fatalf("title length changed at the limit: got %d", len(decoded.Info.Title))
	}
}
