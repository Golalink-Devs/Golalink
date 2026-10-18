package player

import (
	"testing"
	"time"

	"golalink/internal/protocol"
)

// TestSnapshotFreshPlayer pins the defaults a client sees before anything
// happens: the protocol's default volume, no track, an empty filter object the
// client can merge into, and -1 as the "no connection" ping.
func TestSnapshotFreshPlayer(t *testing.T) {
	h := newHarness(t)
	snap := h.Snapshot()

	if snap.GuildID != "guild-id" {
		t.Fatalf("guildId = %q, want guild-id", snap.GuildID)
	}
	if snap.Volume != 100 {
		t.Fatalf("volume = %d, want the protocol default 100", snap.Volume)
	}
	if snap.Paused {
		t.Fatal("a fresh player must not be paused")
	}
	if snap.Track != nil {
		t.Fatalf("track = %+v, want nil", snap.Track)
	}
	if snap.Filters == nil || len(snap.Filters) != 0 {
		t.Fatalf("filters = %#v, want an empty object rather than nil", snap.Filters)
	}
	if snap.State.Connected {
		t.Fatal("a fresh player with no voice connection must report disconnected")
	}
	if snap.State.Ping != -1 {
		t.Fatalf("ping = %d, want the -1 sentinel", snap.State.Ping)
	}
	if snap.State.Position != 0 {
		t.Fatalf("position = %d, want 0", snap.State.Position)
	}
	if now := time.Now().UnixMilli(); snap.State.Time < now-5_000 || snap.State.Time > now+1_000 {
		t.Fatalf("time = %d, want the current time in milliseconds", snap.State.Time)
	}
	if snap.Voice.ChannelID != nil {
		t.Fatalf("channel = %q, want none", *snap.Voice.ChannelID)
	}
}

// TestSnapshotPausedPlayer checks the pause flag is visible before a track
// exists, because a client pauses the player and then learns that from the
// snapshot.
func TestSnapshotPausedPlayer(t *testing.T) {
	h := newHarness(t)
	h.apply(t, protocol.PlayerUpdate{Paused: flag(true)})
	if snap := h.Snapshot(); !snap.Paused {
		t.Fatal("paused player reported as playing")
	}
	h.apply(t, protocol.PlayerUpdate{Paused: flag(false)})
	if snap := h.Snapshot(); snap.Paused {
		t.Fatal("resumed player reported as paused")
	}
}

// TestSnapshotReportsTrackPosition checks the position a client reads is the one
// playback reached, and that the track's own info agrees with it: a client draws
// a progress bar from either field.
func TestSnapshotReportsTrackPosition(t *testing.T) {
	h := newHarness(t)
	h.play(t, seekableTrack(t, "one", "One", 120_000))
	h.apply(t, protocol.PlayerUpdate{Position: i64(12_000)})

	snap := h.Snapshot()
	if snap.Track == nil || snap.Track.Info.Identifier != "one" {
		t.Fatalf("track = %+v, want one", snap.Track)
	}
	if snap.State.Position != 12_000 || snap.Track.Info.Position != 12_000 {
		t.Fatalf("position = %d, track position = %d, want 12000 in both", snap.State.Position, snap.Track.Info.Position)
	}
}

// TestSnapshotClonesState checks a snapshot is a copy: a caller that edits the
// returned track or filters must not change the player for the next reader.
func TestSnapshotClonesState(t *testing.T) {
	h := newHarness(t)
	filters := protocol.Object{"karaoke": map[string]any{"level": 0.5}}
	h.apply(t, protocol.PlayerUpdate{Filters: &filters})
	h.play(t, seekableTrack(t, "one", "One", 60_000))

	first := h.Snapshot()
	if first.Track == nil {
		t.Fatal("snapshot dropped the track")
	}
	first.Track.Info.Title = "mutated"
	first.Track.UserData["requestedBy"] = "mutated"
	karaoke, ok := first.Filters["karaoke"].(map[string]any)
	if !ok {
		t.Fatalf("filter = %#v, want an object", first.Filters["karaoke"])
	}
	karaoke["level"] = 1.0

	second := h.Snapshot()
	if second.Track == nil || second.Track.Info.Identifier != "one" || second.Track.Info.Title != "One" {
		t.Fatalf("track = %+v, want the stored track", second.Track)
	}
	if len(second.Track.UserData) != 0 {
		t.Fatalf("user data = %v, want the stored empty object", second.Track.UserData)
	}
	if level := filterNumber(t, second.Filters, "karaoke", "level"); level != 0.5 {
		t.Fatalf("karaoke level = %v, want the stored 0.5", level)
	}
}

// TestSnapshotConnectedAndPing checks the connection fields come from the live
// transport: connected follows it, and the ping is the transport's own value
// rather than a guess.
func TestSnapshotConnectedAndPing(t *testing.T) {
	h := newHarness(t)
	conn := h.connect(t, channelA)

	snap := h.Snapshot()
	if !snap.State.Connected {
		t.Fatal("a connected player reported disconnected")
	}
	if snap.State.Ping != 42 {
		t.Fatalf("ping = %d, want the connection's 42", snap.State.Ping)
	}
	if snap.Voice.ChannelID == nil || *snap.Voice.ChannelID != channelA {
		t.Fatalf("channel = %v, want %s", snap.Voice.ChannelID, channelA)
	}

	conn.setPing(7)
	if ping := h.Snapshot().State.Ping; ping != 7 {
		t.Fatalf("ping = %d, want 7", ping)
	}

	// A dead transport is reported as disconnected with the sentinel ping, so a
	// client does not show a stale latency forever.
	conn.disconnect()
	snap = h.Snapshot()
	if snap.State.Connected {
		t.Fatal("a dropped connection reported as connected")
	}
	if snap.State.Ping != -1 {
		t.Fatalf("ping = %d, want -1", snap.State.Ping)
	}
}
