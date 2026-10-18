package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golalink/internal/config"
	"golalink/internal/protocol"
)

// event decodes an event the way the worker does, so the embed builders see the
// same map shapes they see in production instead of hand-built Go values.
func event(t *testing.T, raw string) protocol.Object {
	t.Helper()
	var e protocol.Object
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("decode event %s: %v", raw, err)
	}
	return e
}

// embed returns the single embed a payload carries, failing when the body is not
// the shape Discord accepts.
func embed(t *testing.T, payload protocol.Object) protocol.Object {
	t.Helper()
	embeds, ok := payload["embeds"].([]protocol.Object)
	if !ok || len(embeds) != 1 {
		t.Fatalf("payload is not one embed: %#v", payload)
	}
	return embeds[0]
}

func lookupField(emb protocol.Object, name string) (string, bool) {
	fields, _ := emb["fields"].([]protocol.Object)
	for _, field := range fields {
		if field["name"] == name {
			value, _ := field["value"].(string)
			return value, true
		}
	}
	return "", false
}

func field(t *testing.T, emb protocol.Object, name string) string {
	t.Helper()
	value, ok := lookupField(emb, name)
	if !ok {
		t.Fatalf("embed has no %q field: %#v", name, emb["fields"])
	}
	return value
}

func checkEnvelope(t *testing.T, emb protocol.Object, title string, color int) {
	t.Helper()
	if got := emb["title"]; got != title {
		t.Fatalf("title is %v, want %q", got, title)
	}
	if got := emb["color"]; got != color {
		t.Fatalf("color is %v, want %#x", got, color)
	}
	footer, _ := emb["footer"].(protocol.Object)
	if footer["text"] != "Golalink" {
		t.Fatalf("footer is %#v", emb["footer"])
	}
	stamp, _ := emb["timestamp"].(string)
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", stamp, err)
	}
}

// TestIsDiscord pins which endpoints get an embed. The distinction is made from
// the configured URL alone, so a misdetection changes every payload the node
// sends.
func TestIsDiscord(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "discord.com webhook", raw: "https://discord.com/api/webhooks/123/token", want: true},
		{name: "discordapp.com webhook", raw: "https://discordapp.com/api/webhooks/123/token", want: true},
		{name: "discord subdomain", raw: "https://canary.discord.com/api/webhooks/1/x", want: true},
		{name: "host is case insensitive", raw: "HTTPS://DISCORD.COM/api/webhooks/1/x", want: true},
		{name: "discord host without the webhook path", raw: "https://discord.com/channels/1/2", want: true},
		{name: "lookalike host", raw: "https://discord.com.evil.example/steal", want: false},
		{name: "webhook path on another host", raw: "https://notdiscord.example/api/webhooks/1/x", want: true},
		{name: "ordinary endpoint", raw: "https://example.com/hooks/123", want: false},
		{name: "unparsable", raw: "://missing scheme", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDiscord(c.raw); got != c.want {
				t.Fatalf("isDiscord(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestDiscordPayloadTrackStart pins the embed a started track produces: the
// headline track in the description and the identifying details as fields.
func TestDiscordPayloadTrackStart(t *testing.T) {
	payload := discordPayload(event(t, `{
		"type": "TrackStartEvent",
		"guildId": "1181835162864816128",
		"track": {
			"encoded": "abc",
			"info": {
				"identifier": "dQw4w9WgXcQ",
				"isSeekable": true,
				"author": "Rick Astley",
				"length": 213000,
				"isStream": false,
				"position": 0,
				"title": "Never Gonna Give You Up",
				"uri": "https://example.com/watch",
				"sourceName": "youtube"
			}
		}
	}`))
	emb := embed(t, payload)
	checkEnvelope(t, emb, "Track Started", colorStart)

	if got, want := emb["description"], "**Never Gonna Give You Up** — Rick Astley"; got != want {
		t.Fatalf("description is %q, want %q", got, want)
	}
	for name, want := range map[string]string{
		"Source":   "youtube",
		"Guild":    "1181835162864816128",
		"URI":      "https://example.com/watch",
		"Duration": "3:33",
	} {
		if got := field(t, emb, name); got != want {
			t.Fatalf("%s is %q, want %q", name, got, want)
		}
	}
	// Details this event does not carry must not turn into empty fields, which
	// Discord renders as blank rows.
	for _, name := range []string{"Reason", "Code", "Exception", "Threshold"} {
		if got, ok := lookupField(emb, name); ok {
			t.Fatalf("track start has an unexpected %s field: %q", name, got)
		}
	}
}

// TestDiscordPayloadFailures covers the two events an operator has to act on, so
// the cause and the threshold reach the channel instead of only the log.
func TestDiscordPayloadFailures(t *testing.T) {
	t.Run("stuck threshold", func(t *testing.T) {
		emb := embed(t, discordPayload(event(t, `{
			"type": "TrackStuckEvent",
			"guildId": "1",
			"thresholdMs": 10000,
			"track": {"info": {"title": "Song", "author": "Artist", "length": 61000, "sourceName": "youtube"}}
		}`)))
		checkEnvelope(t, emb, "Track Stuck", colorFault)
		if got, want := field(t, emb, "Threshold"), "10000 ms"; got != want {
			t.Fatalf("threshold is %q, want %q", got, want)
		}
		if got, want := field(t, emb, "Duration"), "1:01"; got != want {
			t.Fatalf("duration is %q, want %q", got, want)
		}
	})

	t.Run("exception", func(t *testing.T) {
		emb := embed(t, discordPayload(event(t, `{
			"type": "TrackExceptionEvent",
			"guildId": "1",
			"track": {"info": {"title": "Song", "author": "Artist", "length": 30000, "sourceName": "youtube"}},
			"exception": {"message": "Something went wrong", "severity": "common", "cause": "java.lang.RuntimeException"}
		}`)))
		checkEnvelope(t, emb, "Track Exception", colorFault)
		if got, want := field(t, emb, "Exception"), "Something went wrong"; got != want {
			t.Fatalf("exception is %q, want %q", got, want)
		}
		if got, want := field(t, emb, "Cause"), "java.lang.RuntimeException"; got != want {
			t.Fatalf("cause is %q, want %q", got, want)
		}
	})
}

// TestDiscordPayloadVoiceEvent covers an event with no track: the guild and the
// voice channel are the useful details, and a false boolean is still shown
// because "by remote" being false is information.
func TestDiscordPayloadVoiceEvent(t *testing.T) {
	emb := embed(t, discordPayload(event(t, `{
		"type": "BotConnectEvent",
		"guildId": "1",
		"channelId": "99",
		"byRemote": false
	}`)))
	checkEnvelope(t, emb, "Bot Joined Voice", colorStart)
	if got, want := field(t, emb, "Guild"), "1"; got != want {
		t.Fatalf("guild is %q, want %q", got, want)
	}
	if got, want := field(t, emb, "Channel"), "99"; got != want {
		t.Fatalf("channel is %q, want %q", got, want)
	}
	if got, want := field(t, emb, "By remote"), "false"; got != want {
		t.Fatalf("by remote is %q, want %q", got, want)
	}
	if _, ok := emb["description"]; ok {
		t.Fatalf("a trackless event has a description: %v", emb["description"])
	}
}

// TestDiscordPayloadWithoutDetails keeps the empty embed valid: an unrecognised
// event still reaches the channel with a heading, and no fields key appears when
// there is nothing to list.
func TestDiscordPayloadWithoutDetails(t *testing.T) {
	emb := embed(t, discordPayload(event(t, `{"type": "SomeServerEvent"}`)))
	checkEnvelope(t, emb, "SomeServerEvent", colorDefault)
	if _, ok := emb["fields"]; ok {
		t.Fatalf("empty event has fields: %#v", emb["fields"])
	}
}

// TestDiscordPayloadPlayerState pins the node-wide report: the description says
// how busy the node is, and each player becomes a field naming its guild and
// track and describing its position and voice state.
func TestDiscordPayloadPlayerState(t *testing.T) {
	payload := discordPayload(event(t, `{
		"type": "PlayerStateUpdate",
		"players": 3,
		"playingPlayers": 1,
		"sessions": 1,
		"states": [
			{"guildId": "1", "title": "Song", "position": 65000, "length": 213000, "paused": false, "volume": 80, "connected": true, "ping": 15},
			{"guildId": "2", "volume": 100, "connected": false}
		]
	}`))
	emb := embed(t, payload)
	checkEnvelope(t, emb, "Player State", colorState)
	if got, want := emb["description"], "3 players · 1 playing · 1 session"; got != want {
		t.Fatalf("description is %q, want %q", got, want)
	}
	for name, want := range map[string]string{
		"1 · Song": "1:05/3:33 · playing · vol 80 · ping 15",
		"2":        "idle · vol 100 · disconnected",
	} {
		if got := field(t, emb, name); got != want {
			t.Fatalf("%s is %q, want %q", name, got, want)
		}
	}
}

// TestPlayerStateFieldsTruncatesAtDiscordLimit pins the ceiling: Discord rejects
// an embed with more fields than maxEmbedFields, so a busy node gets one final
// field counting the players it could not show.
func TestPlayerStateFieldsTruncatesAtDiscordLimit(t *testing.T) {
	states := make([]any, 0, 30)
	for i := 0; i < 30; i++ {
		states = append(states, map[string]any{"guildId": fmt.Sprintf("g%d", i)})
	}
	fields := playerStateFields(states)
	if len(fields) != maxEmbedFields {
		t.Fatalf("rendered %d fields, want %d", len(fields), maxEmbedFields)
	}
	if got, want := fields[0]["name"], "g0"; got != want {
		t.Fatalf("first field is %v, want %q", got, want)
	}
	last := fields[len(fields)-1]
	if got, want := last["name"], "…"; got != want {
		t.Fatalf("last field is %v, want %q", got, want)
	}
	if got, want := last["value"], "6 more players"; got != want {
		t.Fatalf("last field is %v, want %q", got, want)
	}
}

// TestPlayerStateFieldsSkipsNonObjects keeps a malformed entry from shifting the
// count or panicking the embed builder.
func TestPlayerStateFieldsSkipsNonObjects(t *testing.T) {
	fields := playerStateFields([]any{"bogus", map[string]any{"guildId": "1"}})
	if len(fields) != 1 {
		t.Fatalf("rendered %d fields, want 1: %#v", len(fields), fields)
	}
	if got, want := fields[0]["name"], "1"; got != want {
		t.Fatalf("field is %v, want %q", got, want)
	}
}

// TestPlayerStateName pins the field heading: the guild first, then what it is
// playing, with a fallback so a field is never unnamed.
func TestPlayerStateName(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]any
		want  string
	}{
		{name: "guild and track", state: map[string]any{"guildId": "1", "title": "Song"}, want: "1 · Song"},
		{name: "idle player", state: map[string]any{"guildId": "1"}, want: "1"},
		{name: "padded guild id", state: map[string]any{"guildId": " 1 ", "title": "Song"}, want: "1 · Song"},
		{name: "nothing to show", state: map[string]any{}, want: "Player"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := playerStateName(c.state); got != c.want {
				t.Fatalf("name is %q, want %q", got, c.want)
			}
		})
	}
}

// TestPlayerStateValue pins what one player field says. The order is part of the
// shape an operator reads, and an idle player must be labelled rather than
// rendered as an empty value.
func TestPlayerStateValue(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]any
		want  string
	}{
		{
			name:  "idle and connected",
			state: map[string]any{"volume": float64(100), "connected": true, "ping": float64(42)},
			want:  "idle · vol 100 · ping 42",
		},
		{
			name: "playing",
			state: map[string]any{
				"title": "Song", "position": float64(65000), "length": float64(213000),
				"paused": false, "volume": float64(80), "connected": true, "ping": float64(15),
			},
			want: "1:05/3:33 · playing · vol 80 · ping 15",
		},
		{
			name:  "paused",
			state: map[string]any{"title": "Song", "position": float64(0), "length": float64(0), "paused": true},
			want:  "0:00 · paused",
		},
		{
			name: "disconnected",
			state: map[string]any{
				"title": "Song", "position": float64(1000), "length": float64(61000),
				"paused": false, "connected": false,
			},
			want: "0:01/1:01 · playing · disconnected",
		},
		{
			name:  "no ping and no volume",
			state: map[string]any{"title": "Song", "position": float64(1000), "paused": false, "ping": float64(-1)},
			want:  "0:01 · playing",
		},
		{
			name:  "no track",
			state: map[string]any{},
			want:  "idle",
		},
		{
			name:  "track without state details",
			state: map[string]any{"title": "Song"},
			want:  "—",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := playerStateValue(c.state); got != c.want {
				t.Fatalf("value is %q, want %q", got, c.want)
			}
		})
	}
}

// TestEventTitleAndColor pins every heading and accent. Discord clients group a
// channel by title and colour, so a changed mapping changes what an operator
// sees even though nothing errors.
func TestEventTitleAndColor(t *testing.T) {
	cases := []struct {
		kind  string
		title string
		color int
	}{
		{kind: "TrackStartEvent", title: "Track Started", color: colorStart},
		{kind: "TrackEndEvent", title: "Track Ended", color: colorEnd},
		{kind: "TrackExceptionEvent", title: "Track Exception", color: colorFault},
		{kind: "TrackStuckEvent", title: "Track Stuck", color: colorFault},
		{kind: "WebSocketClosedEvent", title: "Voice WebSocket Closed", color: colorDefault},
		{kind: "BotConnectEvent", title: "Bot Joined Voice", color: colorStart},
		{kind: "BotDisconnectEvent", title: "Bot Left Voice", color: colorEnd},
		{kind: "", title: "Golalink Event", color: colorDefault},
		{kind: "SomeCustomEvent", title: "SomeCustomEvent", color: colorDefault},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			if got := eventTitle(c.kind); got != c.title {
				t.Fatalf("title is %q, want %q", got, c.title)
			}
			if got := eventColor(c.kind); got != c.color {
				t.Fatalf("color is %#x, want %#x", got, c.color)
			}
		})
	}
}

// TestEmbedValue pins how a decoded JSON value becomes embed text, because a
// changed rendering of numbers or booleans silently changes every field.
func TestEmbedValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{name: "null", in: nil, want: ""},
		{name: "padded string", in: "  hi ", want: "hi"},
		{name: "true", in: true, want: "true"},
		{name: "false", in: false, want: "false"},
		{name: "integral float", in: float64(42), want: "42"},
		{name: "fractional float", in: 1.5, want: "1.5"},
		{name: "unexpected shape", in: []string{"a", "b"}, want: "[a b]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := embedValue(c.in); got != c.want {
				t.Fatalf("embedValue(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTruncate pins the limit as a count of runes: Discord counts characters, so
// cutting bytes would mangle a CJK title and cutting at limit+1 would be
// rejected.
func TestTruncate(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "under the limit", in: "hello", limit: 10, want: "hello"},
		{name: "exactly the limit", in: "hello", limit: 5, want: "hello"},
		{name: "over the limit", in: "hello", limit: 4, want: "hel…"},
		{name: "multibyte text", in: "日本語のタイトル", limit: 4, want: "日本語…"},
		{name: "emoji", in: "🎵🎵🎵", limit: 2, want: "🎵…"},
		{name: "single rune", in: "hello", limit: 1, want: "h"},
		{name: "zero", in: "hello", limit: 0, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncate(c.in, c.limit)
			if got != c.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
			}
		})
	}
}

// TestFormatDuration pins the clock format an embed shows for a track position
// or length, including the rounded-up second and the hour form.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "zero", in: 0, want: "0:00"},
		{name: "seconds", in: 5 * time.Second, want: "0:05"},
		{name: "minutes", in: 65 * time.Second, want: "1:05"},
		{name: "rounds up to the next second", in: 59*time.Second + 600*time.Millisecond, want: "1:00"},
		{name: "hours", in: time.Hour + 2*time.Minute + 3*time.Second, want: "1:02:03"},
		{name: "past a day", in: 25 * time.Hour, want: "25:00:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatDuration(c.in); got != c.want {
				t.Fatalf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestPlural(t *testing.T) {
	cases := []struct {
		count string
		noun  string
		want  string
	}{
		{count: "0", noun: "player", want: "0 players"},
		{count: "1", noun: "player", want: "1 player"},
		{count: "1", noun: "session", want: "1 session"},
		{count: "2", noun: "session", want: "2 sessions"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := plural(c.count, c.noun); got != c.want {
				t.Fatalf("plural(%q, %q) = %q, want %q", c.count, c.noun, got, c.want)
			}
		})
	}
}

// TestNumber pins that only JSON-decoded numbers count: a hand-built int would
// be ignored, which is why the builders are tested through decoded events.
func TestNumber(t *testing.T) {
	if got, ok := number(float64(3)); !ok || got != 3 {
		t.Fatalf("number(float64(3)) = %v, %v", got, ok)
	}
	if _, ok := number(3); ok {
		t.Fatal("number(int(3)) reported a number")
	}
	if _, ok := number("3"); ok {
		t.Fatal("number(string) reported a number")
	}
}

// TestSendWithoutWebhookDoesNothing pins the nil client: a node with the webhook
// off must not deliver anything, and every caller may Send without checking.
func TestSendWithoutWebhookDoesNothing(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// A URL left in the configuration must stay unused while the webhook is off;
	// otherwise disabling it would not stop the posts.
	client := New(config.Webhook{URL: srv.URL}, nil)
	if client != nil {
		t.Fatal("a disabled webhook produced a client")
	}
	if err := client.Send(protocol.Object{"type": "TrackStartEvent"}); err != nil {
		t.Fatalf("send on a nil client: %v", err)
	}
	// Send returns before it marshals, so a value JSON cannot represent is not an
	// error on a node without a webhook either.
	if err := client.Send(make(chan int)); err != nil {
		t.Fatalf("send on a nil client with an unencodable event: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("a node without a webhook posted %d request(s)", n)
	}
	// Enable without a URL is still unconfigured.
	if client := New(config.Webhook{Enable: true, URL: "   "}, nil); client != nil {
		t.Fatal("a blank webhook URL produced a client")
	}
}
