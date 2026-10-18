package player

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"golalink/internal/audio"
	"golalink/internal/protocol"
	"golalink/internal/voice"
)

// Two channel ids that pass voice.Validate; only the string is ever compared, no
// gateway sees them.
const (
	channelA = "111111111111111111"
	channelB = "222222222222222222"
)

// stubLoader stands in for the source manager. It records the identifiers the
// player asked for and answers with whatever the test configured, so resolving a
// track never reaches a network source.
type stubLoader struct {
	mu     sync.Mutex
	calls  []string
	result protocol.LoadResult
	err    error
}

func (l *stubLoader) Load(_ context.Context, identifier string) (protocol.LoadResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, identifier)
	return l.result, l.err
}

// resolve makes the next lookup answer with this load result or error.
func (l *stubLoader) resolve(result protocol.LoadResult, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.result, l.err = result, err
}

func (l *stubLoader) identifiers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// engineStart is one call the player made into the engine. Position is recorded
// because it is the only visible proof that playback resumes where it stopped.
type engineStart struct {
	Track    *protocol.Track
	Position int64
	Volume   int
	Filters  protocol.Object
}

var errNoFrames = errors.New("test engine has no frames")

// stubEngine replaces the decoder pipeline: no FFmpeg process, no audio device,
// only frames the test chooses. A test configures startFn before playback
// begins; it is called from the player's playback goroutine.
type stubEngine struct {
	started chan engineStart
	startFn func(context.Context, engineStart) (audio.Frames, error)
}

func (e *stubEngine) Start(ctx context.Context, t *protocol.Track, position int64, volume int, filters protocol.Object) (audio.Frames, error) {
	start := engineStart{Track: t, Position: position, Volume: volume, Filters: filters}
	if e.started != nil {
		select {
		case e.started <- start:
		default:
		}
	}
	if e.startFn != nil {
		return e.startFn(ctx, start)
	}
	return nil, errNoFrames
}

// toneFrames hands out a frame every time it is asked. Pacing and position
// arithmetic are the only clocks the player applies, so counting reads is a
// stable observation rather than a timing one.
type toneFrames struct {
	mu    sync.Mutex
	reads int
}

func (f *toneFrames) Next() ([]byte, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	return make([]byte, 16), nil
}

func (f *toneFrames) Close() error { return nil }

func (f *toneFrames) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// eofFrames ends the track as soon as the decoder is opened, which is what a
// successfully loaded but empty stream looks like.
type eofFrames struct{}

func (eofFrames) Next() ([]byte, error) { return nil, io.EOF }
func (eofFrames) Close() error          { return nil }

// blockedFrames models a decoder that has nothing to deliver until playback is
// stopped. It shows that pause, seek and destroy do not depend on the decoder
// ever producing a frame.
type blockedFrames struct {
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

func newBlockedFrames(ctx context.Context) *blockedFrames { return &blockedFrames{ctx: ctx} }

func (f *blockedFrames) Next() ([]byte, error) {
	<-f.ctx.Done()
	return nil, f.ctx.Err()
}

func (f *blockedFrames) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *blockedFrames) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeConn is the voice connection a real client would own: it records what the
// player wrote and whether it was speaking, without a gateway or a UDP socket.
type fakeConn struct {
	mu        sync.Mutex
	connected bool
	ping      int64
	closed    bool
	writes    int
	wrote     chan int
	spoke     chan bool
}

func newFakeConn() *fakeConn {
	return &fakeConn{connected: true, ping: 42, wrote: make(chan int, 64), spoke: make(chan bool, 64)}
}

func (c *fakeConn) Write(frame []byte) error {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return errors.New("voice is disconnected")
	}
	c.writes++
	n := c.writes
	c.mu.Unlock()
	select {
	case c.wrote <- n:
	default:
	}
	return nil
}

func (c *fakeConn) Speaking(_ context.Context, on bool) error {
	select {
	case c.spoke <- on:
	default:
	}
	return nil
}

func (c *fakeConn) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected && !c.closed
}

func (c *fakeConn) Ping() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.closed {
		return -1
	}
	return c.ping
}

func (c *fakeConn) Close() {
	c.mu.Lock()
	c.closed = true
	c.connected = false
	c.mu.Unlock()
}

func (c *fakeConn) setPing(ping int64) {
	c.mu.Lock()
	c.ping = ping
	c.mu.Unlock()
}

// disconnect simulates the transport dying, which is different from the player
// closing it: the callback is what has to tell the player.
func (c *fakeConn) disconnect() {
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
}

func (c *fakeConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// waitWrites blocks until at least n frames reached the connection. The timeout
// only exists so a stuck player fails the test instead of hanging the suite.
func (c *fakeConn) waitWrites(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for c.writeCount() < n {
		select {
		case <-c.wrote:
		case <-deadline:
			t.Fatalf("timed out waiting for %d writes, saw %d", n, c.writeCount())
		}
	}
}

// waitSpeaking blocks until the player sent the given speaking flag.
func (c *fakeConn) waitSpeaking(t *testing.T, want bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case on := <-c.spoke:
			if on == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for speaking=%v", want)
		}
	}
}

// factoryCall is one request the player made to join a channel. It pins that the
// credentials a client sent reach the transport unchanged.
type factoryCall struct {
	User  string
	Guild string
	Voice protocol.VoiceState
}

// fakeFactory hands out fake connections and remembers each request, including
// the close callback so a test can act as the transport's failure path.
type fakeFactory struct {
	mu       sync.Mutex
	requests []factoryCall
	conns    []*fakeConn
	onClose  []func(voice.Closed)
	err      error
}

func (f *fakeFactory) Connect(_ context.Context, user, guild string, state protocol.VoiceState, onClose func(voice.Closed)) (voice.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, factoryCall{User: user, Guild: guild, Voice: state})
	if f.err != nil {
		return nil, f.err
	}
	conn := newFakeConn()
	f.conns = append(f.conns, conn)
	f.onClose = append(f.onClose, onClose)
	return conn, nil
}

// refuse makes every join attempt fail with err.
func (f *fakeFactory) refuse(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeFactory) calls() []factoryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factoryCall(nil), f.requests...)
}

func (f *fakeFactory) connections() []*fakeConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeConn(nil), f.conns...)
}

// closeHandler returns the callback the player registered for the i-th
// connection, which is how a test imitates the transport closing.
func (f *fakeFactory) closeHandler(i int) func(voice.Closed) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.onClose) {
		return nil
	}
	return f.onClose[i]
}

// harness owns a Player wired to fakes plus the events it emitted. Waiting for a
// specific event is how the asynchronous parts are observed without sleeping.
type harness struct {
	*Player
	engine   *stubEngine
	loader   *stubLoader
	factory  *fakeFactory
	counters *Counters
	events   chan protocol.Object

	mu   sync.Mutex
	seen []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		engine:   &stubEngine{started: make(chan engineStart, 16)},
		loader:   &stubLoader{},
		factory:  &fakeFactory{},
		counters: &Counters{},
		events:   make(chan protocol.Object, 256),
	}
	h.Player = New("bot-user", "guild-id", h.engine, h.loader, h.factory, h.record, h.counters)
	t.Cleanup(h.Destroy)
	return h
}

// record keeps the emission order for failure messages and queues the event for
// the test that is waiting on it. Both are non-blocking: the player emits while
// holding its lock, so an emit that waited could deadlock it.
func (h *harness) record(v any) {
	fields, ok := v.(protocol.Object)
	if !ok {
		return
	}
	h.mu.Lock()
	h.seen = append(h.seen, fmt.Sprint(fields["type"]))
	h.mu.Unlock()
	select {
	case h.events <- fields:
	default:
	}
}

func (h *harness) types() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// waitEvent returns the next event of the given type, skipping the others.
func (h *harness) waitEvent(t *testing.T, kind string) protocol.Object {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-h.events:
			if event["type"] == kind {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; emitted so far: %v", kind, h.types())
		}
	}
}

// drain removes and returns the events already queued. It is only meaningful for
// events a call emits synchronously, which is every event outside voice.
func (h *harness) drain() []protocol.Object {
	var events []protocol.Object
	for {
		select {
		case event := <-h.events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// apply runs an update that is expected to be accepted.
func (h *harness) apply(t *testing.T, update protocol.PlayerUpdate) protocol.Player {
	t.Helper()
	state, err := h.Update(context.Background(), update, false)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return state
}

// play starts a track the way a client does: a single encoded value with no
// separate info beside it.
func (h *harness) play(t *testing.T, track *protocol.Track) protocol.Player {
	t.Helper()
	return h.apply(t, encodedUpdate(t, track))
}

// connect asks the player to join a channel and returns the fake connection the
// factory handed back, once the join has been announced.
func (h *harness) connect(t *testing.T, channel string) *fakeConn {
	t.Helper()
	h.apply(t, voiceUpdate(channel))
	h.waitEvent(t, "BotConnectEvent")
	conns := h.factory.connections()
	if len(conns) == 0 {
		t.Fatal("factory returned no connection")
	}
	return conns[len(conns)-1]
}

// waitStart returns the next engine start.
func (h *harness) waitStart(t *testing.T) engineStart {
	t.Helper()
	return waitChan(t, h.engine.started, "engine start")
}

func waitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// seekableTrack builds the encoded value a client sends. The player decodes it
// instead of trusting the info beside it, so a track only exists here if its
// encoding is valid.
func seekableTrack(t *testing.T, identifier, title string, length int64) *protocol.Track {
	t.Helper()
	return seekableTrackAt(t, identifier, title, length, 0)
}

// seekableTrackAt is seekableTrack with a starting position, which a client uses
// to resume a track where it left off.
func seekableTrackAt(t *testing.T, identifier, title string, length, position int64) *protocol.Track {
	t.Helper()
	track, err := protocol.Encode(protocol.TrackInfo{
		Identifier: identifier,
		Title:      title,
		Author:     "author",
		Length:     length,
		Position:   position,
		SourceName: "http",
		URI:        protocol.String("https://example.test/" + identifier),
	})
	if err != nil {
		t.Fatalf("encoding %q: %v", identifier, err)
	}
	return track
}

// streamTrack has no meaningful length and is not seekable, the way a live
// stream behaves.
func streamTrack(t *testing.T, identifier string) *protocol.Track {
	t.Helper()
	track, err := protocol.Encode(protocol.TrackInfo{
		Identifier: identifier,
		Title:      "live",
		IsStream:   true,
		SourceName: "http",
	})
	if err != nil {
		t.Fatalf("encoding %q: %v", identifier, err)
	}
	return track
}

// encodedRaw wraps an encoded track as the JSON string a request carries.
func encodedRaw(t *testing.T, track *protocol.Track) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(track.Encoded)
	if err != nil {
		t.Fatalf("wrapping %q: %v", track.Info.Identifier, err)
	}
	return raw
}

func encodedUpdate(t *testing.T, track *protocol.Track) protocol.PlayerUpdate {
	t.Helper()
	return protocol.PlayerUpdate{EncodedTrack: encodedRaw(t, track)}
}

// stopUpdate clears the current track, which is how a client asks for silence
// while keeping the player and its voice connection.
func stopUpdate() protocol.PlayerUpdate {
	return protocol.PlayerUpdate{EncodedTrack: json.RawMessage("null")}
}

// i64, flag and str build the update fields the protocol models as pointers, so
// "absent" and "zero" stay distinguishable.
func i64(n int64) *protocol.Int64 { v := protocol.Int64(n); return &v }
func flag(b bool) *bool           { return &b }
func str(s string) *string        { return &s }

func voiceState(channel string) protocol.VoiceState {
	return protocol.VoiceState{
		Token:     "session-token",
		SessionID: "session-id",
		Endpoint:  "c-iad21.discord.media:443",
		ChannelID: str(channel),
	}
}

// voiceUpdate asks the player to join a channel.
func voiceUpdate(channel string) protocol.PlayerUpdate {
	state := voiceState(channel)
	return protocol.PlayerUpdate{Voice: &state}
}

// eventKinds lists the types in emission order, which is how a test compares a
// synchronous burst against what it expects.
func eventKinds(events []protocol.Object) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, fmt.Sprint(event["type"]))
	}
	return kinds
}

func assertKinds(t *testing.T, events []protocol.Object, want ...string) {
	t.Helper()
	if got := eventKinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// eventTrack returns the track an event reported. A track end without one would
// be useless to a client, so this fails rather than skipping the assertion.
func eventTrack(t *testing.T, event protocol.Object) *protocol.Track {
	t.Helper()
	track, ok := event["track"].(*protocol.Track)
	if !ok || track == nil {
		t.Fatalf("event %v carries no track", event)
	}
	return track
}

// filterNumber reads a nested numeric filter field, failing rather than panicking
// when the shape is wrong.
func filterNumber(t *testing.T, filters protocol.Object, filter, field string) float64 {
	t.Helper()
	object, ok := filters[filter].(map[string]any)
	if !ok {
		t.Fatalf("filter %q = %#v, want an object", filter, filters[filter])
	}
	n, ok := object[field].(float64)
	if !ok {
		t.Fatalf("filter %q.%s = %#v, want a number", filter, field, object[field])
	}
	return n
}
