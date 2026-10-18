package player

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golalink/internal/protocol"
)

// TestUpdateVolumeAndFilters checks the settings that survive a request without
// touching the track, and that a rejected request leaves the old settings alone.
func TestUpdateVolumeAndFilters(t *testing.T) {
	h := newHarness(t)
	filters := protocol.Object{"timescale": map[string]any{"speed": 1.5}}
	state := h.apply(t, protocol.PlayerUpdate{Volume: i64(35), Filters: &filters})
	if state.Volume != 35 {
		t.Fatalf("volume = %d, want 35", state.Volume)
	}
	if speed := filterNumber(t, state.Filters, "timescale", "speed"); speed != 1.5 {
		t.Fatalf("timescale speed = %v, want 1.5", speed)
	}

	// One bad field must fail the request, not half-apply it: a client that sent
	// a volume of 1001 and a valid filter would otherwise hear the filter.
	if _, err := h.Update(context.Background(), protocol.PlayerUpdate{Volume: i64(1001)}, false); err == nil {
		t.Fatal("volume above the ceiling was accepted")
	}
	if volume := h.Snapshot().Volume; volume != 35 {
		t.Fatalf("rejected volume changed the player: %d", volume)
	}
	bad := protocol.Object{"bogus": 1.0}
	if _, err := h.Update(context.Background(), protocol.PlayerUpdate{Filters: &bad}, false); err == nil {
		t.Fatal("unknown filter was accepted")
	}
	if speed := filterNumber(t, h.Snapshot().Filters, "timescale", "speed"); speed != 1.5 {
		t.Fatalf("rejected filters changed the player: %v", speed)
	}

	if events := h.drain(); len(events) != 0 {
		t.Fatalf("volume and filter changes must not report track events: %v", eventKinds(events))
	}
}

// TestUpdatePauseSemantics pins when the pause flag moves. The play path is the
// subtle one: an absent "paused" unpauses, because that is what the reference
// implementation's setPause(paused ?: false) does.
func TestUpdatePauseSemantics(t *testing.T) {
	h := newHarness(t)
	if state := h.apply(t, protocol.PlayerUpdate{Paused: flag(true)}); !state.Paused {
		t.Fatal("pause was not applied")
	}
	if state := h.apply(t, protocol.PlayerUpdate{Paused: flag(false)}); state.Paused {
		t.Fatal("resume was not applied")
	}

	h.apply(t, protocol.PlayerUpdate{Paused: flag(true)})
	if state := h.play(t, seekableTrack(t, "one", "One", 60_000)); state.Paused {
		t.Fatal("a new track should start playing when the request did not ask to stay paused")
	}

	pausedPlay := encodedUpdate(t, seekableTrack(t, "two", "Two", 60_000))
	pausedPlay.Paused = flag(true)
	if state := h.apply(t, pausedPlay); !state.Paused {
		t.Fatal("an explicit pause on a play request was ignored")
	}

	// A play request skipped by noReplace is not a play at all, so it must not
	// move the pause flag either.
	attempt := encodedUpdate(t, seekableTrack(t, "three", "Three", 60_000))
	attempt.Paused = flag(false)
	state, err := h.Update(context.Background(), attempt, true)
	if err != nil {
		t.Fatalf("noReplace play: %v", err)
	}
	if !state.Paused {
		t.Fatal("a skipped noReplace request unpaused the player")
	}
	if state.Track == nil || state.Track.Info.Identifier != "two" {
		t.Fatalf("a skipped noReplace request replaced the track: %+v", state.Track)
	}
}

// TestUpdatePlaysEncodedTrack covers what a client sees when playback is replaced
// or stopped: the previous track is reported with the reason that explains it,
// and the new one starts at the beginning.
func TestUpdatePlaysEncodedTrack(t *testing.T) {
	h := newHarness(t)
	state := h.play(t, seekableTrack(t, "one", "One", 60_000))
	if state.Track == nil || state.Track.Info.Identifier != "one" || state.Track.Info.Title != "One" {
		t.Fatalf("track = %+v, want the encoded one", state.Track)
	}
	if state.State.Position != 0 || state.Paused {
		t.Fatalf("position = %d, paused = %v, want a fresh start", state.State.Position, state.Paused)
	}
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("starting on an empty player must not report an end: %v", eventKinds(events))
	}

	state = h.play(t, seekableTrack(t, "two", "Two", 60_000))
	events := h.drain()
	assertKinds(t, events, "TrackEndEvent")
	if events[0]["reason"] != "replaced" {
		t.Fatalf("reason = %v, want replaced", events[0]["reason"])
	}
	if track := eventTrack(t, events[0]); track.Info.Identifier != "one" {
		t.Fatalf("replaced track = %q, want the one that was playing", track.Info.Identifier)
	}
	if state.Track == nil || state.Track.Info.Identifier != "two" {
		t.Fatalf("track = %+v, want two", state.Track)
	}

	// Null encoded clears the track rather than naming one, and the client is
	// told the track stopped instead of finished.
	state = h.apply(t, stopUpdate())
	if state.Track != nil {
		t.Fatalf("track = %+v, want nil after clearing", state.Track)
	}
	events = h.drain()
	assertKinds(t, events, "TrackEndEvent")
	if events[0]["reason"] != "stopped" {
		t.Fatalf("reason = %v, want stopped", events[0]["reason"])
	}
	if track := eventTrack(t, events[0]); track.Info.Identifier != "two" {
		t.Fatalf("stopped track = %q, want two", track.Info.Identifier)
	}
}

// TestUpdateStartingPositionFromTrack checks that a track carries its own start
// offset and that playing a second track does not inherit the first one's
// position.
func TestUpdateStartingPositionFromTrack(t *testing.T) {
	h := newHarness(t)
	state := h.apply(t, encodedUpdate(t, seekableTrackAt(t, "first", "First", 120_000, 4_000)))
	if state.State.Position != 4_000 {
		t.Fatalf("position = %d, want the track's own 4000", state.State.Position)
	}
	if state.Track == nil || state.Track.Info.Position != 4_000 {
		t.Fatalf("track position = %+v, want 4000", state.Track)
	}

	state = h.apply(t, encodedUpdate(t, seekableTrackAt(t, "second", "Second", 120_000, 0)))
	if state.State.Position != 0 {
		t.Fatalf("position = %d, want 0 for a track with no offset", state.State.Position)
	}
}

// TestUpdateSeekPosition covers the seek rules: it needs a track, it needs a
// seekable one, and it cannot move past the end of the track.
func TestUpdateSeekPosition(t *testing.T) {
	h := newHarness(t)
	h.play(t, seekableTrack(t, "one", "One", 200_000))

	state := h.apply(t, protocol.PlayerUpdate{Position: i64(50_000)})
	if state.Track == nil {
		t.Fatal("seek dropped the track")
	}
	if state.State.Position != 50_000 || state.Track.Info.Position != 50_000 {
		t.Fatalf("position = %d, want 50000 in both the state and the track", state.State.Position)
	}

	// A seek past the end lands on the end, which is where playback would stop
	// anyway; leaving it beyond the length would make position reports disagree
	// with the track.
	state = h.apply(t, protocol.PlayerUpdate{Position: i64(300_000)})
	if state.State.Position != 200_000 {
		t.Fatalf("position = %d, want the track length 200000", state.State.Position)
	}

	if _, err := h.Update(context.Background(), protocol.PlayerUpdate{Position: i64(-1)}, false); err == nil || !strings.Contains(err.Error(), "nonnegative") {
		t.Fatalf("negative seek: got %v, want a validation error", err)
	}
	if position := h.Snapshot().State.Position; position != 200_000 {
		t.Fatalf("rejected seek moved the player to %d", position)
	}

	// A position with nothing to seek is accepted and has nothing to move.
	empty := newHarness(t)
	if state := empty.apply(t, protocol.PlayerUpdate{Position: i64(5_000)}); state.State.Position != 0 {
		t.Fatalf("position on an idle player = %d, want 0", state.State.Position)
	}

	// A live stream has no timeline to seek, so the request is accepted but has
	// no effect rather than being an error.
	live := newHarness(t)
	live.play(t, streamTrack(t, "live"))
	if state := live.apply(t, protocol.PlayerUpdate{Position: i64(5_000)}); state.State.Position != 0 {
		t.Fatalf("position on a stream = %d, want 0", state.State.Position)
	}
}

// TestUpdateNoReplace covers the request a client sends when it does not want to
// interrupt playback: with something playing the play is skipped entirely, and
// with nothing playing it still works.
func TestUpdateNoReplace(t *testing.T) {
	h := newHarness(t)
	h.play(t, seekableTrack(t, "one", "One", 60_000))
	h.apply(t, protocol.PlayerUpdate{Paused: flag(true)})

	attempt := encodedUpdate(t, seekableTrack(t, "two", "Two", 60_000))
	attempt.Paused = flag(false)
	state, err := h.Update(context.Background(), attempt, true)
	if err != nil {
		t.Fatalf("noReplace play: %v", err)
	}
	if state.Track == nil || state.Track.Info.Identifier != "one" {
		t.Fatalf("track = %+v, want the playing one", state.Track)
	}
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("a skipped play must not report an end: %v", eventKinds(events))
	}

	h.apply(t, stopUpdate())
	if events := h.drain(); len(events) != 1 {
		t.Fatalf("clearing the track reported %v, want one end", eventKinds(events))
	}

	// noReplace is not a veto on starting: with the player empty the track loads.
	state, err = h.Update(context.Background(), encodedUpdate(t, seekableTrack(t, "two", "Two", 60_000)), true)
	if err != nil {
		t.Fatalf("noReplace play on an empty player: %v", err)
	}
	if state.Track == nil || state.Track.Info.Identifier != "two" {
		t.Fatalf("track = %+v, want two", state.Track)
	}
	if state, err = h.Update(context.Background(), encodedUpdate(t, seekableTrack(t, "three", "Three", 60_000)), true); err != nil {
		t.Fatalf("noReplace play: %v", err)
	}
	if state.Track == nil || state.Track.Info.Identifier != "two" {
		t.Fatalf("noReplace replaced a playing track: %+v", state.Track)
	}
}

// TestUpdateUserData pins the metadata a client sets on a track: it travels with
// the play request, and changing it alone must not restart or end the track.
func TestUpdateUserData(t *testing.T) {
	h := newHarness(t)
	track := seekableTrack(t, "one", "One", 60_000)
	state := h.apply(t, protocol.PlayerUpdate{Track: &protocol.TrackUpdate{
		Encoded:  encodedRaw(t, track),
		UserData: protocol.Object{"requestedBy": "listener"},
	}})
	if state.Track == nil || state.Track.UserData["requestedBy"] != "listener" {
		t.Fatalf("user data = %v, want the value from the play request", state.Track)
	}

	state = h.apply(t, protocol.PlayerUpdate{Track: &protocol.TrackUpdate{
		UserData: protocol.Object{"requestedBy": "someone else"},
	}})
	if state.Track == nil || state.Track.Info.Identifier != "one" {
		t.Fatalf("track = %+v, want the same track", state.Track)
	}
	if state.Track.UserData["requestedBy"] != "someone else" {
		t.Fatalf("user data = %v, want the replacement", state.Track.UserData)
	}
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("a metadata update must not report an end: %v", eventKinds(events))
	}
}

// TestUpdateLoadFailure covers the identifier path: what a client is told when a
// lookup fails, and that a failed lookup leaves playback untouched.
func TestUpdateLoadFailure(t *testing.T) {
	h := newHarness(t)
	h.play(t, seekableTrack(t, "one", "One", 60_000))

	loadErr := errors.New("source is unreachable")
	h.loader.resolve(protocol.LoadResult{}, loadErr)
	_, err := h.Update(context.Background(), protocol.PlayerUpdate{Identifier: str("missing")}, false)
	if !errors.Is(err, loadErr) {
		t.Fatalf("load error = %v, want the loader's error", err)
	}
	if state := h.Snapshot(); state.Track == nil || state.Track.Info.Identifier != "one" {
		t.Fatalf("a failed load replaced the track: %+v", state.Track)
	}
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("a failed load must not report an end: %v", eventKinds(events))
	}

	// An identifier that resolves to a playlist names more than one track, and
	// the player has no queue to put it in.
	h.loader.resolve(protocol.LoadResult{LoadType: "playlist", Data: &protocol.Playlist{Tracks: []*protocol.Track{seekableTrack(t, "two", "Two", 60_000)}}}, nil)
	_, err = h.Update(context.Background(), protocol.PlayerUpdate{Identifier: str("list")}, false)
	if err == nil || !strings.Contains(err.Error(), "one track") {
		t.Fatalf("playlist load: got %v, want a single-track error", err)
	}
	if state := h.Snapshot(); state.Track == nil || state.Track.Info.Identifier != "one" {
		t.Fatalf("a playlist load replaced the track: %+v", state.Track)
	}

	resolved := seekableTrackAt(t, "resolved", "Resolved", 30_000, 2_500)
	h.loader.resolve(protocol.LoadResult{LoadType: "track", Data: resolved}, nil)
	state := h.apply(t, protocol.PlayerUpdate{Identifier: str("resolved")})
	if state.Track == nil || state.Track.Info.Identifier != "resolved" {
		t.Fatalf("track = %+v, want the loaded one", state.Track)
	}
	if state.State.Position != 2_500 {
		t.Fatalf("position = %d, want the loaded track's own 2500", state.State.Position)
	}

	// An empty identifier names nothing, so it must be refused without asking
	// the loader to search for the empty string.
	before := len(h.loader.identifiers())
	if _, err := h.Update(context.Background(), protocol.PlayerUpdate{Identifier: str("")}, false); err == nil {
		t.Fatal("empty identifier was accepted")
	}
	if after := h.loader.identifiers(); len(after) != before {
		t.Fatalf("empty identifier reached the loader: %v", after)
	}
}

// TestUpdateAfterDestroy pins that a destroyed player is inert: a late request
// must fail instead of resurrecting playback or a voice connection.
func TestUpdateAfterDestroy(t *testing.T) {
	h := newHarness(t)
	h.Destroy()
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("destroying an idle player reported %v", eventKinds(events))
	}

	_, err := h.Update(context.Background(), stopUpdate(), false)
	if err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("update after destroy: got %v, want a destroyed-player error", err)
	}
}
