package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golalink/internal/player"
	"golalink/internal/protocol"
)

type session struct {
	id, user          string
	mu                sync.Mutex
	players           map[string]*player.Player
	resuming          bool
	timeout           int
	attaching         bool // an upgrade in progress, reserved before releasing the server lock
	connected, closed bool
	disconnected      time.Time
	conn              *websocket.Conn
	events            chan []byte
	done              chan struct{}
	failOnce          sync.Once
	server            *Server
}

func (s *session) emit(v any) {
	b, e := json.Marshal(v)
	if e != nil {
		s.server.deps.Logger.Warn("event could not be encoded", "sessionId", s.id, "error", e)
		return
	}
	// Debug logging names the event and the fields an operator looks for when a
	// client reports odd behaviour: why a track ended, how long a stall lasted,
	// and why a voice connection closed.
	if s.server.deps.Logger.Enabled(context.Background(), slog.LevelDebug) {
		fields := []any{"sessionId", s.id}
		if obj, ok := v.(protocol.Object); ok {
			for _, key := range []string{"type", "guildId", "reason", "code", "byRemote", "thresholdMs"} {
				if val, ok := obj[key]; ok {
					fields = append(fields, key, val)
				}
			}
		}
		s.server.deps.Logger.Debug("event dispatched", fields...)
	}
	// The webhook sees the event whether or not a client is still listening, and
	// the event is also a chance to refresh the node-wide summary.
	s.server.deps.Webhook.Send(v)
	s.server.requestReport()
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.events <- b:
	default:
		s.failOnce.Do(func() {
			s.server.deps.Logger.Warn("event buffer overflow; closing session because the client is not reading",
				"sessionId", s.id,
				"buffer", cap(s.events),
			)
			go s.server.remove(s.id)
		})
	}
}
func (s *session) snapshots() []protocol.Player {
	s.mu.Lock()
	ps := make([]*player.Player, 0, len(s.players))
	for _, p := range s.players {
		ps = append(ps, p)
	}
	s.mu.Unlock()
	out := make([]protocol.Player, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Snapshot())
	}
	return out
}
func (s *session) destroy() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	c := s.conn
	ps := s.players
	s.players = map[string]*player.Player{}
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	for _, p := range ps {
		p.Destroy()
		s.server.playerCount.Add(-1)
	}
}
func (s *session) writeLoop(c *websocket.Conn, stop <-chan struct{}) {
	updates := time.NewTicker(time.Duration(s.server.config.PlayerUpdateSeconds) * time.Second)
	defer updates.Stop()
	stats := time.NewTicker(time.Duration(s.server.config.StatsSeconds) * time.Second)
	defer stats.Stop()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	defer c.Close()
	previousSent := s.server.counters.Sent.Load()
	previousDeficit := s.server.counters.Deficit.Load()
	write := func(v any) error { _ = c.SetWriteDeadline(time.Now().Add(5 * time.Second)); return c.WriteJSON(v) }
	for {
		select {
		case <-stop:
			return
		case <-s.done:
			return
		case b := <-s.events:
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if c.WriteMessage(websocket.TextMessage, b) != nil {
				return
			}
		case <-updates.C:
			for _, p := range s.snapshots() {
				if write(protocol.Object{"op": "playerUpdate", "guildId": p.GuildID, "state": p.State}) != nil {
					return
				}
			}
		case <-stats.C:
			v := s.server.stats()
			v["op"] = "stats"
			sent, deficit := s.server.counters.Sent.Load(), s.server.counters.Deficit.Load()
			v["frameStats"] = protocol.Object{"sent": sent - previousSent, "nulled": 0, "deficit": deficit - previousDeficit}
			previousSent, previousDeficit = sent, deficit
			if write(v) != nil {
				return
			}
		case <-ping.C:
			if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		}
	}
}
