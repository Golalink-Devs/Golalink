package player

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golalink/internal/audio"
	"golalink/internal/protocol"
	"golalink/internal/voice"
)

// TestPlaybackConnectsAndTearsDown covers the join and the teardown: the bot is
// announced once in the channel it joined, playback starts at position zero with
// the player's volume, and destroying reports the track as cleaned up and the
// bot as gone.
func TestPlaybackConnectsAndTearsDown(t *testing.T) {
	h := newHarness(t)
	frames := make(chan *blockedFrames, 1)
	h.engine.startFn = func(ctx context.Context, _ engineStart) (audio.Frames, error) {
		decoder := newBlockedFrames(ctx)
		frames <- decoder
		return decoder, nil
	}

	h.apply(t, voiceUpdate(channelA))
	joined := h.waitEvent(t, "BotConnectEvent")
	if joined["channelId"] != channelA {
		t.Fatalf("join channel = %v, want %s", joined["channelId"], channelA)
	}
	// The event is readable by anyone the webhook is shared with, so the voice
	// credentials must not ride along with it.
	for _, secret := range []string{"token", "sessionId", "endpoint"} {
		if _, ok := joined[secret]; ok {
			t.Fatalf("join event leaked %q: %v", secret, joined)
		}
	}
	conns := h.factory.connections()
	if len(conns) != 1 {
		t.Fatalf("connections = %d, want 1", len(conns))
	}
	conn := conns[0]

	h.play(t, seekableTrack(t, "one", "One", 60_000))
	start := h.waitStart(t)
	if start.Position != 0 || start.Volume != 100 {
		t.Fatalf("start = position %d volume %d, want 0 and 100", start.Position, start.Volume)
	}
	if start.Track == nil || start.Track.Info.Identifier != "one" {
		t.Fatalf("start track = %+v, want one", start.Track)
	}
	decoder := waitChan(t, frames, "decoder frames")
	h.Destroy()
	events := h.drain()
	assertKinds(t, events, "TrackEndEvent", "BotDisconnectEvent")
	if events[0]["reason"] != "cleanup" {
		t.Fatalf("track end reason = %v, want cleanup", events[0]["reason"])
	}
	if track := eventTrack(t, events[0]); track.Info.Identifier != "one" {
		t.Fatalf("ended track = %q, want one", track.Info.Identifier)
	}
	if events[1]["reason"] != "destroyed" {
		t.Fatalf("disconnect reason = %v, want destroyed", events[1]["reason"])
	}
	if events[1]["channelId"] != channelA {
		t.Fatalf("disconnect channel = %v, want %s", events[1]["channelId"], channelA)
	}
	if !conn.isClosed() || !decoder.isClosed() {
		t.Fatal("destroy left the connection or the decoder open")
	}
	if snap := h.Snapshot(); snap.Track != nil || snap.State.Connected {
		t.Fatalf("destroyed player snapshot = track %+v connected %v", snap.Track, snap.State.Connected)
	}

	// Destroy is idempotent: a second call must not report the same departure
	// twice, which would leave a client thinking it had joined and left twice.
	h.Destroy()
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("second destroy reported %v", eventKinds(events))
	}
}

// TestPlaybackEngineFailure covers how a decoder that fails, stalls or simply
// ends is reported. The exception and the end have to agree, because a client
// uses the reason to decide whether to retry.
func TestPlaybackEngineFailure(t *testing.T) {
	t.Run("exception", func(t *testing.T) {
		h := newHarness(t)
		boom := errors.New("decoder exploded")
		h.engine.startFn = func(context.Context, engineStart) (audio.Frames, error) { return nil, boom }
		h.connect(t, channelA)
		h.play(t, seekableTrack(t, "one", "One", 60_000))

		event := h.waitEvent(t, "TrackExceptionEvent")
		if track := eventTrack(t, event); track.Info.Identifier != "one" {
			t.Fatalf("failed track = %q, want one", track.Info.Identifier)
		}
		exception, ok := event["exception"].(protocol.Exception)
		if !ok {
			t.Fatalf("exception = %#v, want a protocol exception", event["exception"])
		}
		if exception.Severity != "fault" || exception.Cause != "golalink.audio" {
			t.Fatalf("exception = %+v, want a fault from golalink.audio", exception)
		}
		if !strings.Contains(exception.Message, "decoder exploded") {
			t.Fatalf("exception message = %q, want the decoder's error", exception.Message)
		}

		end := h.waitEvent(t, "TrackEndEvent")
		if end["reason"] != "loadFailed" {
			t.Fatalf("track end reason = %v, want loadFailed", end["reason"])
		}
		if h.counters.Errors.Load() != 1 {
			t.Fatalf("errors = %d, want 1", h.counters.Errors.Load())
		}
		if h.counters.Sent.Load() != 0 {
			t.Fatalf("sent = %d, want none", h.counters.Sent.Load())
		}
		if snap := h.Snapshot(); snap.Track != nil {
			t.Fatalf("track = %+v, want nil after a failed load", snap.Track)
		}
	})

	t.Run("stuck", func(t *testing.T) {
		h := newHarness(t)
		h.engine.startFn = func(context.Context, engineStart) (audio.Frames, error) { return nil, audio.ErrStuck }
		h.connect(t, channelA)
		h.play(t, seekableTrack(t, "one", "One", 60_000))

		event := h.waitEvent(t, "TrackStuckEvent")
		if event["thresholdMs"] != audio.StuckThreshold.Milliseconds() {
			t.Fatalf("threshold = %v, want %d", event["thresholdMs"], audio.StuckThreshold.Milliseconds())
		}
		if end := h.waitEvent(t, "TrackEndEvent"); end["reason"] != "loadFailed" {
			t.Fatalf("track end reason = %v, want loadFailed", end["reason"])
		}
		if h.counters.Errors.Load() != 1 {
			t.Fatalf("errors = %d, want 1", h.counters.Errors.Load())
		}
	})

	t.Run("end of stream", func(t *testing.T) {
		h := newHarness(t)
		h.engine.startFn = func(context.Context, engineStart) (audio.Frames, error) { return eofFrames{}, nil }
		h.connect(t, channelA)
		h.play(t, seekableTrack(t, "one", "One", 60_000))

		end := h.waitEvent(t, "TrackEndEvent")
		if end["reason"] != "finished" {
			t.Fatalf("track end reason = %v, want finished", end["reason"])
		}
		if h.counters.Errors.Load() != 0 {
			t.Fatalf("errors = %d, want none for an ordinary end", h.counters.Errors.Load())
		}
		// A failure is emitted before the end it caused, so nothing may follow.
		for _, event := range h.drain() {
			if kind := event["type"]; kind == "TrackExceptionEvent" || kind == "TrackStuckEvent" {
				t.Fatalf("an ordinary end reported %v", kind)
			}
		}
		if snap := h.Snapshot(); snap.Track != nil {
			t.Fatalf("track = %+v, want nil after the stream ended", snap.Track)
		}
	})
}

// TestPlaybackRestartKeepsPosition checks that changing the volume mid-track
// restarts the decoder where playback stopped rather than at the beginning: the
// replacement run must be handed the current position.
func TestPlaybackRestartKeepsPosition(t *testing.T) {
	h := newHarness(t)
	frames := &toneFrames{}
	second := make(chan engineStart, 1)

	var mu sync.Mutex
	starts := 0
	h.engine.startFn = func(ctx context.Context, start engineStart) (audio.Frames, error) {
		mu.Lock()
		starts++
		n := starts
		mu.Unlock()
		if n == 1 {
			return frames, nil
		}
		// Hold the replacement decoder so nothing can advance the position while
		// the test compares the two numbers.
		second <- start
		<-ctx.Done()
		return nil, ctx.Err()
	}

	conn := h.connect(t, channelA)
	h.play(t, seekableTrack(t, "one", "One", 60_000))
	if start := h.waitStart(t); start.Position != 0 {
		t.Fatalf("first start position = %d, want 0", start.Position)
	}
	conn.waitWrites(t, 3)
	if _, err := h.Update(context.Background(), protocol.PlayerUpdate{Volume: i64(35)}, false); err != nil {
		t.Fatalf("volume update: %v", err)
	}

	// The old run cannot write after the update returns and the new one is held,
	// so the connection's write count is the position the restart must use.
	restart := waitChan(t, second, "restarted decoder")
	want := int64(conn.writeCount()) * 20
	if restart.Position != want {
		t.Fatalf("restart position = %d, want %d", restart.Position, want)
	}
	if restart.Volume != 35 {
		t.Fatalf("restart volume = %d, want 35", restart.Volume)
	}
	if restart.Track == nil || restart.Track.Info.Identifier != "one" {
		t.Fatalf("restart track = %+v, want one", restart.Track)
	}
	if len(restart.Filters) != 0 {
		t.Fatalf("restart filters = %v, want the player's empty set", restart.Filters)
	}
}

// TestPlaybackPauseGatesFrames checks pause in both directions: a player that
// starts paused consumes no source data, and a pause during playback stops the
// frame stream where it is instead of restarting it.
func TestPlaybackPauseGatesFrames(t *testing.T) {
	h := newHarness(t)
	frames := &toneFrames{}
	h.engine.startFn = func(context.Context, engineStart) (audio.Frames, error) { return frames, nil }
	conn := h.connect(t, channelA)

	startPaused := encodedUpdate(t, seekableTrack(t, "one", "One", 60_000))
	startPaused.Paused = flag(true)
	h.apply(t, startPaused)
	h.waitStart(t)
	if reads := frames.readCount(); reads != 0 {
		t.Fatalf("paused player consumed %d frames, want none", reads)
	}
	if writes := conn.writeCount(); writes != 0 {
		t.Fatalf("paused player wrote %d frames, want none", writes)
	}

	h.apply(t, protocol.PlayerUpdate{Paused: flag(false)})
	conn.waitWrites(t, 1)
	if reads := frames.readCount(); reads < 1 {
		t.Fatal("resumed player consumed no frames")
	}

	h.apply(t, protocol.PlayerUpdate{Paused: flag(true)})
	conn.waitSpeaking(t, false)
	pausedWrites := conn.writeCount()
	if position := h.Snapshot().State.Position; position != int64(pausedWrites)*20 {
		t.Fatalf("paused position = %d, want the %d frames actually delivered", position, pausedWrites)
	}
	if !h.Snapshot().Paused {
		t.Fatal("snapshot still reports playing after a pause")
	}

	// Resuming continues from the frame playback reached; the decoder is never
	// rewound behind the pause.
	h.apply(t, protocol.PlayerUpdate{Paused: flag(false)})
	conn.waitWrites(t, pausedWrites+1)
	if position := h.Snapshot().State.Position; position < int64(pausedWrites+1)*20 {
		t.Fatalf("resumed position = %d, want playback to continue past the pause", position)
	}
}

// TestPlaybackDestroyWhileDecoderBlocked checks that destroying a player whose
// decoder never delivers a frame still returns: the goroutine must not outlive
// the player, and the track is reported as cleaned up rather than failed.
func TestPlaybackDestroyWhileDecoderBlocked(t *testing.T) {
	h := newHarness(t)
	decoders := make(chan *blockedFrames, 1)
	h.engine.startFn = func(ctx context.Context, _ engineStart) (audio.Frames, error) {
		decoder := newBlockedFrames(ctx)
		decoders <- decoder
		return decoder, nil
	}
	h.connect(t, channelA)
	h.play(t, seekableTrack(t, "one", "One", 60_000))
	h.waitStart(t)
	decoder := waitChan(t, decoders, "decoder frames")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Destroy()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("destroy blocked on a decoder that never produced a frame")
	}

	events := h.drain()
	assertKinds(t, events, "TrackEndEvent", "BotDisconnectEvent")
	if events[0]["reason"] != "cleanup" {
		t.Fatalf("track end reason = %v, want cleanup", events[0]["reason"])
	}
	if !decoder.isClosed() {
		t.Fatal("destroy left the blocked decoder open")
	}
}

// TestPlaybackConnectionCloseIsReported checks how a transport that dies on its
// own is reported: the bot is announced as gone once, with the close code, and
// the track survives because playback can resume after a reconnect.
func TestPlaybackConnectionCloseIsReported(t *testing.T) {
	h := newHarness(t)
	h.engine.startFn = func(ctx context.Context, _ engineStart) (audio.Frames, error) {
		return newBlockedFrames(ctx), nil
	}
	conn := h.connect(t, channelA)
	h.play(t, seekableTrack(t, "one", "One", 60_000))
	h.waitStart(t)

	onClose := h.factory.closeHandler(0)
	if onClose == nil {
		t.Fatal("the player registered no close callback")
	}
	conn.disconnect()
	onClose(voice.Closed{Code: 4015, Reason: "session invalidated", ByRemote: true})

	gone := h.waitEvent(t, "BotDisconnectEvent")
	if gone["reason"] != "closed" {
		t.Fatalf("disconnect reason = %v, want closed", gone["reason"])
	}
	if gone["code"] != 4015 || gone["byRemote"] != true {
		t.Fatalf("disconnect = code %v byRemote %v, want 4015 and true", gone["code"], gone["byRemote"])
	}
	if gone["channelId"] != channelA {
		t.Fatalf("disconnect channel = %v, want %s", gone["channelId"], channelA)
	}
	closed := h.waitEvent(t, "WebSocketClosedEvent")
	if closed["code"] != 4015 || closed["reason"] != "session invalidated" || closed["byRemote"] != true {
		t.Fatalf("websocket close = %v, want the transport's code and reason", closed)
	}

	snap := h.Snapshot()
	if snap.State.Connected || snap.State.Ping != -1 {
		t.Fatalf("snapshot = connected %v ping %d, want disconnected", snap.State.Connected, snap.State.Ping)
	}
	if snap.Track == nil || snap.Track.Info.Identifier != "one" {
		t.Fatalf("track = %+v, want it kept for the next connection", snap.Track)
	}
	for _, event := range h.drain() {
		if event["type"] == "TrackEndEvent" {
			t.Fatalf("a dropped connection reported the track as ended: %v", event)
		}
	}

	// Destroying after the transport died must not announce the departure twice,
	// but the track it was holding is still cleaned up.
	h.Destroy()
	events := h.drain()
	assertKinds(t, events, "TrackEndEvent")
	if events[0]["reason"] != "cleanup" {
		t.Fatalf("track end reason = %v, want cleanup", events[0]["reason"])
	}
}

// TestVoiceReplacementReportsPreviousChannel checks that moving the bot closes
// the old connection and reports the channel it left, not the one it is joining.
func TestVoiceReplacementReportsPreviousChannel(t *testing.T) {
	h := newHarness(t)
	first := h.connect(t, channelA)

	h.apply(t, voiceUpdate(channelB))
	replaced := h.waitEvent(t, "BotDisconnectEvent")
	if replaced["reason"] != "replaced" {
		t.Fatalf("disconnect reason = %v, want replaced", replaced["reason"])
	}
	if replaced["channelId"] != channelA {
		t.Fatalf("disconnect channel = %v, want the channel being left %s", replaced["channelId"], channelA)
	}
	h.waitEvent(t, "BotConnectEvent")

	if !first.isClosed() {
		t.Fatal("the old connection was not closed")
	}
	snap := h.Snapshot()
	if !snap.State.Connected {
		t.Fatal("the new connection is not reported as connected")
	}
	if snap.Voice.ChannelID == nil || *snap.Voice.ChannelID != channelB {
		t.Fatalf("channel = %v, want %s", snap.Voice.ChannelID, channelB)
	}

	calls := h.factory.calls()
	if len(calls) != 2 {
		t.Fatalf("connect calls = %d, want 2", len(calls))
	}
	if calls[0].User != "bot-user" || calls[0].Guild != "guild-id" {
		t.Fatalf("connect identity = %q/%q, want the player's user and guild", calls[0].User, calls[0].Guild)
	}
	if calls[1].Voice.ChannelID == nil || *calls[1].Voice.ChannelID != channelB {
		t.Fatalf("second connect channel = %v, want %s", calls[1].Voice.ChannelID, channelB)
	}
}

// TestVoiceConnectFailureIsReported checks that a join that never happened is
// reported as a local close (4000) rather than as a Discord close code, and that
// the player does not pretend to be connected.
func TestVoiceConnectFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.factory.refuse(errors.New("gateway refused the connection"))
	h.apply(t, voiceUpdate(channelA))

	event := h.waitEvent(t, "WebSocketClosedEvent")
	if event["code"] != 4000 {
		t.Fatalf("code = %v, want 4000 for a local failure", event["code"])
	}
	if event["reason"] != "gateway refused the connection" || event["byRemote"] != false {
		t.Fatalf("close = %v, want the factory's error as a local close", event)
	}
	if snap := h.Snapshot(); snap.State.Connected || snap.State.Ping != -1 {
		t.Fatalf("snapshot = connected %v ping %d, want disconnected", snap.State.Connected, snap.State.Ping)
	}

	h.Destroy()
	if events := h.drain(); len(events) != 0 {
		t.Fatalf("a join that never happened was reported as a departure: %v", eventKinds(events))
	}
}
