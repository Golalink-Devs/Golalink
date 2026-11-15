package player

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golalink/internal/audio"
	"golalink/internal/audio/dispatch"
	"golalink/internal/audio/pure"
	"golalink/internal/config"
	"golalink/internal/protocol"
	"golalink/internal/source"
)

func TestPlaybackWatchdog(t *testing.T) {
	t.Run("HTTP source sends headers but no media", func(t *testing.T) {
		t.Parallel()
		canceled := make(chan struct{}, 1)
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			canceled <- struct{}{}
		}))
		t.Cleanup(origin.Close)
		cfg := config.Default()
		cfg.AllowPrivateNetworks = true
		src := source.New(cfg)
		t.Cleanup(src.Close)
		h := newHarness(t)
		h.Player.engine = &dispatch.Dispatcher{Source: src, Pure: &pure.Engine{Config: cfg}, Fallback: &audio.FFmpeg{Config: cfg}}
		track, err := protocol.Encode(protocol.TrackInfo{
			Identifier: origin.URL, Title: "Stalled media", URI: &origin.URL, SourceName: "http",
		})
		if err != nil {
			t.Fatal(err)
		}
		h.connect(t, channelA)
		h.play(t, track)
		assertPlaybackStuck(t, h)
		waitChan(t, canceled, "HTTP request cancellation")
	})
	t.Run("decoder opens but never produces a frame", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.engine.startFn = func(ctx context.Context, _ engineStart) (audio.Frames, error) {
			return newBlockedFrames(ctx), nil
		}
		h.connect(t, channelA)
		h.play(t, seekableTrack(t, "one", "One", 60_000))
		assertPlaybackStuck(t, h)
	})
}

func assertPlaybackStuck(t *testing.T, h *harness) {
	t.Helper()
	select {
	case event := <-h.events:
		if event["type"] != "TrackStuckEvent" || event["thresholdMs"] != audio.StuckThreshold.Milliseconds() {
			t.Fatalf("got %v, want TrackStuckEvent after %v", event, audio.StuckThreshold)
		}
	case <-time.After(audio.StuckThreshold + 5*time.Second):
		t.Fatal("stalled playback never reported TrackStuckEvent")
	}
	if event := h.waitEvent(t, "TrackEndEvent"); event["reason"] != "loadFailed" {
		t.Fatalf("track end reason = %v, want loadFailed", event["reason"])
	}
}
