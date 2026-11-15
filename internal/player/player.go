package player

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golalink/internal/audio"
	"golalink/internal/protocol"
	"golalink/internal/voice"
)

type Loader interface {
	Load(context.Context, string) (protocol.LoadResult, error)
}
type Counters struct {
	Sent    atomic.Uint64
	Deficit atomic.Uint64
	Errors  atomic.Uint64
}
type Player struct {
	updateMu        sync.Mutex
	mu              sync.Mutex
	ctx             context.Context
	close           context.CancelFunc
	wg              sync.WaitGroup
	state           protocol.Player
	endTime         *int64
	started, closed bool
	engine          audio.Engine
	loader          Loader
	factory         voice.Factory
	conn            voice.Connection
	connectCancel   context.CancelFunc
	voiceGeneration uint64
	// voiceUp records that a join has been announced and no leave has been
	// announced for it yet, so a dead connection is reported once.
	voiceUp    bool
	cancel     context.CancelFunc
	generation uint64
	notify     chan struct{}
	emit       func(any)
	user       string
	Counters   *Counters
}

func New(user, guild string, engine audio.Engine, loader Loader, factory voice.Factory, emit func(any), counters *Counters) *Player {
	ctx, cancel := context.WithCancel(context.Background())
	return &Player{ctx: ctx, close: cancel, user: user, engine: engine, loader: loader, factory: factory, emit: emit, Counters: counters, notify: make(chan struct{}, 1), state: protocol.Player{GuildID: guild, Volume: 100, Filters: protocol.Object{}, State: protocol.PlayerState{Ping: -1}}}
}
func (p *Player) snapshotLocked() protocol.Player {
	s := p.state
	s.Track = protocol.Clone(s.Track)
	if s.Track != nil {
		s.Track.Info.Position = s.State.Position
	}
	b, _ := json.Marshal(s.Filters)
	s.Filters = nil
	_ = json.Unmarshal(b, &s.Filters)
	s.State.Time = time.Now().UnixMilli()
	s.State.Connected = p.conn != nil && p.conn.Connected()
	s.State.Ping = -1
	if s.State.Connected {
		s.State.Ping = p.conn.Ping()
	}
	return s
}
func (p *Player) Snapshot() protocol.Player {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}
func Validate(u protocol.PlayerUpdate) error {
	if u.Volume != nil && (*u.Volume < 0 || *u.Volume > 1000) {
		return errors.New("volume must be 0..1000")
	}
	if u.Position != nil && *u.Position < 0 {
		return errors.New("position must be nonnegative")
	}
	if len(u.EndTime) > 0 && string(u.EndTime) != "null" {
		if n, err := protocol.ParseInt(u.EndTime); err != nil || n <= 0 {
			return errors.New("endTime must be positive or null")
		}
	}
	if u.Filters != nil {
		if _, _, e := audio.Build(*u.Filters, 100); e != nil {
			return e
		}
	}
	if u.Voice != nil {
		if e := voice.Validate(*u.Voice); e != nil {
			return e
		}
	}
	if u.Track != nil && (len(u.EncodedTrack) > 0 || u.Identifier != nil) {
		return errors.New("track cannot be combined with deprecated track fields")
	}
	t := u.Track
	if t == nil {
		t = &protocol.TrackUpdate{Encoded: u.EncodedTrack, Identifier: u.Identifier}
	}
	if len(t.Encoded) > 0 && t.Identifier != nil {
		return errors.New("encoded and identifier are mutually exclusive")
	}
	if t.Identifier != nil && *t.Identifier == "" {
		return errors.New("identifier must not be empty")
	}
	if len(t.Encoded) > 0 && string(t.Encoded) != "null" {
		var encoded string
		if json.Unmarshal(t.Encoded, &encoded) != nil {
			return errors.New("encoded must be a string or null")
		}
		if _, e := protocol.Decode(encoded); e != nil {
			return e
		}
	}
	return nil
}
func (p *Player) Update(ctx context.Context, u protocol.PlayerUpdate, noReplace bool) (protocol.Player, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	p.updateMu.Lock()
	defer p.updateMu.Unlock()
	if e := Validate(u); e != nil {
		return protocol.Player{}, e
	}
	t := u.Track
	if t == nil {
		t = &protocol.TrackUpdate{Encoded: u.EncodedTrack, Identifier: u.Identifier}
	}
	change := len(t.Encoded) > 0 || t.Identifier != nil
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return protocol.Player{}, errors.New("player is destroyed")
	}
	ignore := noReplace && p.state.Track != nil
	p.mu.Unlock()
	var track *protocol.Track
	if change && !ignore {
		if t.Identifier != nil {
			r, e := p.loader.Load(ctx, *t.Identifier)
			if e != nil {
				return protocol.Player{}, e
			}
			var ok bool
			track, ok = r.Data.(*protocol.Track)
			if r.LoadType != "track" || !ok {
				return protocol.Player{}, errors.New("identifier must resolve to one track")
			}
		} else if string(t.Encoded) != "null" {
			var encoded string
			_ = json.Unmarshal(t.Encoded, &encoded)
			track, _ = protocol.Decode(encoded)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return protocol.Player{}, errors.New("player is destroyed")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Player{}, err
	}
	restart := u.Position != nil || u.Volume != nil || u.Filters != nil
	if change && !ignore {
		p.stopLocked()
		if p.state.Track != nil {
			reason := "replaced"
			if track == nil {
				reason = "stopped"
			}
			p.eventLocked("TrackEndEvent", protocol.Object{"track": p.state.Track, "reason": reason})
		}
		p.state.Track = track
		p.started = false
		p.endTime = nil
		p.state.State.Position = 0
		if track != nil {
			p.state.State.Position = track.Info.Position
		}
		restart = true
	}
	if t.UserData != nil && p.state.Track != nil && !ignore {
		p.state.Track.UserData = t.UserData
	}
	if u.Position != nil && p.state.Track != nil && p.state.Track.Info.IsSeekable {
		p.state.State.Position = int64(*u.Position)
		if p.state.Track.Info.Length > 0 && p.state.State.Position > p.state.Track.Info.Length {
			p.state.State.Position = p.state.Track.Info.Length
		}
	}
	if len(u.EndTime) > 0 {
		p.endTime = nil
		if string(u.EndTime) != "null" {
			if n, err := protocol.ParseInt(u.EndTime); err == nil {
				p.endTime = &n
			}
		}
	}
	if u.Volume != nil {
		p.state.Volume = int(*u.Volume)
	}
	if change && !ignore {
		// Playing a new track starts it unless the request asks to stay paused.
		// The reference implementation calls setPause(paused ?: false) on the
		// play path, so an absent "paused" unpauses rather than preserving the
		// previous state. A play request skipped by noReplace leaves it alone.
		p.state.Paused = u.Paused != nil && *u.Paused
	} else if !change && u.Paused != nil {
		p.state.Paused = *u.Paused
	}
	if u.Filters != nil {
		p.state.Filters = *u.Filters
	}
	if restart {
		p.stopLocked()
	}
	if u.Voice != nil {
		v := *u.Voice
		// The channel being left is the one the previous voice state named,
		// which the update has not overwritten yet.
		left := p.state.Voice.ChannelID
		p.state.Voice = v
		p.stopLocked()
		if p.connectCancel != nil {
			p.connectCancel()
		}
		if p.conn != nil {
			old := p.conn
			p.conn = nil
			old.Close()
		}
		if p.voiceUp {
			p.voiceUp = false
			p.eventLocked("BotDisconnectEvent", botVoiceFields(left, "replaced"))
		}
		p.voiceGeneration++
		gen := p.voiceGeneration
		work, cancel := context.WithCancel(p.ctx)
		p.connectCancel = cancel
		p.wg.Add(1)
		go p.connect(work, v, gen)
	}
	p.startLocked()
	select {
	case p.notify <- struct{}{}:
	default:
	}
	return p.snapshotLocked(), nil
}
func (p *Player) connect(ctx context.Context, v protocol.VoiceState, gen uint64) {
	defer p.wg.Done()
	conn, err := p.factory.Connect(ctx, p.user, p.state.GuildID, v, func(c voice.Closed) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed || p.voiceGeneration != gen {
			return
		}
		p.stopLocked()
		// The bot leaving is reported before the protocol event that explains
		// the close, so a channel reads as a departure followed by its cause.
		if p.voiceUp {
			p.voiceUp = false
			gone := botVoiceFields(p.state.Voice.ChannelID, "closed")
			gone["code"] = c.Code
			gone["byRemote"] = c.ByRemote
			p.eventLocked("BotDisconnectEvent", gone)
		}
		p.eventLocked("WebSocketClosedEvent", protocol.Object{"code": c.Code, "reason": c.Reason, "byRemote": c.ByRemote})
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.voiceGeneration != gen {
		if conn != nil {
			conn.Close()
		}
		return
	}
	if err != nil {
		p.eventLocked("WebSocketClosedEvent", protocol.Object{"code": 4000, "reason": err.Error(), "byRemote": false})
		return
	}
	p.conn = conn
	// A connection that reports itself ready is the moment the bot is
	// actually in the channel, which is what a listener wants to know; a
	// connection that died while it was being opened never gets announced.
	if conn.Connected() {
		p.voiceUp = true
		p.eventLocked("BotConnectEvent", botVoiceFields(v.ChannelID, ""))
	}
	p.startLocked()
}
func (p *Player) eventLocked(kind string, fields protocol.Object) {
	fields["op"] = "event"
	fields["type"] = kind
	fields["guildId"] = p.state.GuildID
	p.emit(fields)
}

// botVoiceFields describes a bot voice event: the channel the connection was
// on and, when it ended, why. The voice token, session and endpoint are left
// out, because an event is readable by anyone the webhook is shared with.
func botVoiceFields(channel *string, reason string) protocol.Object {
	fields := protocol.Object{}
	if channel != nil && *channel != "" {
		fields["channelId"] = *channel
	}
	if reason != "" {
		fields["reason"] = reason
	}
	return fields
}
func (p *Player) stopLocked() {
	p.generation++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
		if p.conn != nil && p.conn.Connected() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = p.conn.Speaking(ctx, false)
			cancel()
		}
	}
}
func (p *Player) startLocked() {
	if p.closed || p.cancel != nil || p.state.Track == nil || p.conn == nil || !p.conn.Connected() {
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.cancel = cancel
	p.generation++
	gen := p.generation
	t := protocol.Clone(p.state.Track)
	pos := p.state.State.Position
	volume := p.state.Volume
	filters := p.snapshotLocked().Filters
	conn := p.conn
	p.wg.Add(1)
	go p.run(ctx, gen, t, pos, volume, filters, conn)
}
func (p *Player) run(ctx context.Context, gen uint64, t *protocol.Track, pos int64, volume int, filters protocol.Object, conn voice.Connection) {
	defer p.wg.Done()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// Use the same context for opening and reading the media, but arm the
	// watchdog only while waiting on the engine, never while paused or pacing.
	fired := make(chan struct{})
	watchdog := time.AfterFunc(audio.StuckThreshold, func() {
		cancel(audio.ErrStuck)
		close(fired)
	})
	checkWatchdog := func(err error) error {
		if !watchdog.Stop() {
			// The callback may still be setting the cancellation cause.
			<-fired
		}
		if errors.Is(context.Cause(ctx), audio.ErrStuck) {
			return audio.ErrStuck
		}
		return err
	}
	frames, err := p.engine.Start(ctx, t, pos, volume, filters)
	if frames != nil {
		defer frames.Close()
	}
	err = checkWatchdog(err)
	if err != nil {
		if ctx.Err() == nil || errors.Is(err, audio.ErrStuck) {
			p.finish(gen, err)
		}
		return
	}
	_, speed, _ := audio.Build(filters, volume)
	position := float64(pos)
	next := time.Now()
	speaking := false
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.generation == gen && speaking {
			work, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = conn.Speaking(work, false)
		}
	}()
	for {
		// Pause consumes neither source data nor playback time. Pipe backpressure bounds memory.
		p.mu.Lock()
		if gen != p.generation || p.closed {
			p.mu.Unlock()
			return
		}
		paused := p.state.Paused
		end := p.endTime != nil && p.state.State.Position >= *p.endTime
		p.mu.Unlock()
		if end {
			p.finish(gen, nil)
			return
		}
		if paused {
			if speaking {
				_ = conn.Speaking(ctx, false)
				speaking = false
			}
			select {
			case <-ctx.Done():
				return
			case <-p.notify:
				next = time.Now()
				continue
			}
		}
		watchdog.Reset(audio.StuckThreshold)
		frame, e := frames.Next()
		e = checkWatchdog(e)
		if e != nil {
			if ctx.Err() != nil && !errors.Is(e, audio.ErrStuck) {
				return
			}
			if e == io.EOF {
				e = nil
			}
			p.finish(gen, e)
			return
		}
		for {
			wait := time.Until(next)
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			p.mu.Lock()
			if gen != p.generation || p.closed {
				p.mu.Unlock()
				return
			}
			if p.state.Paused {
				p.mu.Unlock()
				if speaking {
					_ = conn.Speaking(ctx, false)
					speaking = false
				}
				select {
				case <-ctx.Done():
					return
				case <-p.notify:
					next = time.Now()
					continue
				}
			}
			if !speaking {
				if e = conn.Speaking(ctx, true); e != nil {
					p.mu.Unlock()
					p.finish(gen, e)
					return
				}
				speaking = true
			}
			if e = conn.Write(frame); e != nil {
				p.mu.Unlock()
				p.finish(gen, e)
				return
			}
			p.Counters.Sent.Add(1)
			position += 20 * speed
			p.state.State.Position = int64(position)
			if !p.started {
				p.started = true
				p.eventLocked("TrackStartEvent", protocol.Object{"track": protocol.Clone(p.state.Track)})
			}
			p.mu.Unlock()
			break
		}
		next = next.Add(20 * time.Millisecond)
		if late := time.Since(next); late > 100*time.Millisecond {
			p.Counters.Deficit.Add(uint64(late / (20 * time.Millisecond)))
			next = time.Now()
		}
	}
}
func (p *Player) finish(gen uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.generation || p.closed || p.state.Track == nil {
		return
	}
	reason := "finished"
	if err != nil {
		reason = "loadFailed"
		p.Counters.Errors.Add(1)
		if errors.Is(err, audio.ErrStuck) {
			p.eventLocked("TrackStuckEvent", protocol.Object{"track": protocol.Clone(p.state.Track), "thresholdMs": audio.StuckThreshold.Milliseconds()})
		} else {
			p.eventLocked("TrackExceptionEvent", protocol.Object{"track": protocol.Clone(p.state.Track), "exception": protocol.Exception{Message: fmt.Sprint(err), Severity: "fault", Cause: "golalink.audio"}})
		}
	}
	p.eventLocked("TrackEndEvent", protocol.Object{"track": protocol.Clone(p.state.Track), "reason": reason})
	p.state.Track = nil
	p.cancel = nil
	p.started = false
}
func (p *Player) Destroy() {
	// Cancel a source lookup before waiting for the serialized update lock.
	p.close()
	p.updateMu.Lock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.updateMu.Unlock()
		return
	}
	p.closed = true
	p.close()
	p.stopLocked()
	if p.state.Track != nil {
		p.eventLocked("TrackEndEvent", protocol.Object{"track": protocol.Clone(p.state.Track), "reason": "cleanup"})
	}
	p.state.Track = nil
	conn := p.conn
	p.conn = nil
	if p.voiceUp {
		p.voiceUp = false
		p.eventLocked("BotDisconnectEvent", botVoiceFields(p.state.Voice.ChannelID, "destroyed"))
	}
	p.mu.Unlock()
	p.updateMu.Unlock()
	if conn != nil {
		conn.Close()
	}
	p.wg.Wait()
}
