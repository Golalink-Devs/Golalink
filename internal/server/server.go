package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golalink/internal/audio"
	"golalink/internal/config"
	"golalink/internal/lyrics"
	"golalink/internal/middleware"
	"golalink/internal/player"
	"golalink/internal/protocol"
	"golalink/internal/system"
	"golalink/internal/voice"
	"golalink/internal/webhook"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

// Version is the node's own version. It is a variable rather than a constant so
// a release build can stamp the tag into it.
// APIVersion is the Lavalink API this node implements.
const (
	Version    = "0.1.0"
	APIVersion = "4.2.2"
)

type Dependencies struct {
	Loader player.Loader
	Engine audio.Engine
	Voice  voice.Factory
	Logger *slog.Logger
	// Lyrics is the lyrics manager. Nil leaves the lyrics routes answering with
	// no content, which is what a node without the plugin does.
	Lyrics *lyrics.Manager
	// Webhook receives every player event, plus a report of the state of every
	// player on the node, spaced by the configured latency. Nil starts nothing.
	Webhook *webhook.Client
}
type Server struct {
	config      config.Config
	deps        Dependencies
	mu          sync.Mutex
	sessions    map[string]*session
	closed      bool
	done        chan struct{}
	wg          sync.WaitGroup
	cleanup     sync.WaitGroup
	cpu         system.CPU
	started     time.Time
	playerCount atomic.Int64
	requests    atomic.Uint64
	counters    player.Counters
	lyrics      *lyrics.Manager
	// reports holds one pending request for a player state report, and
	// lastReport is when the last one went out; stateLatencySeconds spaces them.
	reports    chan struct{}
	reportMu   sync.Mutex
	lastReport time.Time
	mux        *http.ServeMux
}

func New(c config.Config, d Dependencies) (*Server, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if d.Loader == nil || d.Engine == nil || d.Voice == nil {
		return nil, errors.New("source, audio and voice dependencies are required")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	s := &Server{config: c, deps: d, sessions: map[string]*session{}, done: make(chan struct{}), started: time.Now(), lyrics: d.Lyrics, mux: http.NewServeMux(), reports: make(chan struct{}, 1)}
	s.routes()
	s.wg.Add(1)
	go s.reap()
	// A node with no webhook has nothing to report to, so it starts no worker.
	if d.Webhook != nil {
		s.wg.Add(1)
		go s.reportLoop()
	}
	return s, nil
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		// Lavalink labels every response with this header, and clients use it to
		// tell a v4 node from a v3 one.
		w.Header().Set("Lavalink-Api-Version", "4")
		w.Header().Set("Server", "Golalink/"+Version)
		// Every request is accounted for: successful ones at debug, rejected
		// ones at warn, and server faults at error, so an operator sees
		// failures without turning on debug logging.
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			level := slog.LevelDebug
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}
			fields := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", rec.status,
				"bytes", rec.written,
				"remote", clientHost(r.RemoteAddr),
				"durationMs", time.Since(start).Milliseconds(),
			}
			if rec.reason != "" {
				fields = append(fields, "error", rec.reason)
			}
			s.deps.Logger.Log(r.Context(), level, "request", fields...)
		}()
		s.mux.ServeHTTP(rec, r)
	})
}

// authorize rejects a request whose Authorization header does not carry the
// node password. It runs before the route's rate limiter so rejected credentials
// cannot spend the tokens shared by authenticated clients.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(s.config.Password)) != 1 {
			s.fail(w, r, http.StatusUnauthorized, "invalid authorization")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter records what was sent so the request log can report it. The
// WebSocket upgrade needs the extra interfaces of the original writer, so they
// are forwarded rather than dropped.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int
	// reason is what fail said when it rejected the request, so the log line for
	// it carries the reason and not only the status. A client that reports a
	// status on its own leaves an operator guessing.
	reason string
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	return n, err
}
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return h.Hijack()
}

// clientHost strips the port from a remote address for logging.
func clientHost(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func (s *Server) routes() {
	// deny answers a throttled request in the same JSON shape as every other
	// error, so a client parses one format.
	deny := func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, http.StatusTooManyRequests, "too many requests")
	}
	// protected checks the password before applying the route's rate limiter.
	// public applies only the limiter, since it has no password check.
	protected := func(handler http.HandlerFunc, r rate.Limit, burst int) http.Handler {
		if s.config.RateLimits.Enable {
			return s.authorize(middleware.RateLimitMiddleware(r, burst, deny)(handler))
		}
		return s.authorize(handler)
	}
	public := func(handler http.HandlerFunc, r rate.Limit, burst int) http.Handler {
		if s.config.RateLimits.Enable {
			return middleware.RateLimitMiddleware(r, burst, deny)(handler)
		}
		return handler
	}
	register := func(pattern string, handler http.HandlerFunc, r rate.Limit, burst int) {
		s.mux.Handle(pattern, protected(handler, r, burst))
	}
	registerPublic := func(pattern string, handler http.HandlerFunc, r rate.Limit, burst int) {
		s.mux.Handle(pattern, public(handler, r, burst))
	}

	registerPublic("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, APIVersion)
	}, 2, 5)

	registerPublic("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.json(w, http.StatusOK, protocol.Object{
			"status":  "ok",
			"name":    "Golalink",
			"version": Version,
		})
	}, 5, 10)

	register("GET /v4/info", s.info, 2, 5)

	register("GET /v4/stats", func(w http.ResponseWriter, r *http.Request) {
		s.json(w, http.StatusOK, s.stats())
	}, 1, 3)

	register("GET /v4/metrics", s.metrics, 1, 2)
	register("GET /metrics", s.metrics, 1, 2)

	register("GET /v4/websocket", s.websocket, 1, 3)

	register("GET /v4/loadtracks", s.loadTracks, 2, 4)
	register("GET /v4/decodetrack", s.decodeTrack, 5, 10)
	register("POST /v4/decodetracks", s.decodeTracks, 1, 2)

	register("GET /v4/lyrics", s.lyricsForEncodedTrack, 2, 4)

	register(
		"GET /v4/sessions/{sessionId}/players/{guildId}/track/lyrics",
		s.lyricsForPlayer, 2, 4,
	)

	register("PATCH /v4/sessions/{sessionId}", s.updateSession, 5, 10)

	register(
		"GET /v4/sessions/{sessionId}/players",
		s.getPlayers, 10, 15,
	)
	register(
		"GET /v4/sessions/{sessionId}/players/{guildId}",
		s.getPlayer, 10, 15,
	)
	register(
		"PATCH /v4/sessions/{sessionId}/players/{guildId}",
		s.updatePlayer, 5, 10,
	)
	register(
		"DELETE /v4/sessions/{sessionId}/players/{guildId}",
		s.deletePlayer, 3, 5,
	)

	register("GET /v4/routeplanner/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}, 1, 3)

	register("POST /v4/routeplanner/free/{rest}", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, http.StatusNotImplemented, "IP route planning is not enabled")
	}, 1, 2)

	register("GET /", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, http.StatusNotFound, "route not found")
	}, 2, 5)
}
func (s *Server) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if rec, ok := w.(*statusWriter); ok {
		rec.reason = msg
	}
	s.json(w, status, protocol.Object{"timestamp": time.Now().UnixMilli(), "status": status, "error": http.StatusText(status), "message": msg, "path": r.URL.Path})
}
func (s *Server) decode(w http.ResponseWriter, r *http.Request, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(dest); err != nil {
		s.fail(w, r, 400, "invalid JSON body: "+err.Error())
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		s.fail(w, r, 400, "expected one JSON value")
		return false
	}
	return true
}

// getSession resolves a session for the REST API. Upstream keeps a disconnected
// but resumable session in a separate map from the live sessions, and its REST
// lookup only reads the live map, so every REST route reports "Session not
// found" until the client reconnects. That is matched here.
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) *session {
	s.mu.Lock()
	v := s.sessions[r.PathValue("sessionId")]
	s.mu.Unlock()
	if v == nil || !v.attached() {
		s.fail(w, r, 404, "session not found")
		return nil
	}
	return v
}

// A pending upgrade or a disconnected session is hidden from REST.
func (v *session) attached() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.connected && !v.closed
}
func (s *Server) loadTracks(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("identifier")
	if id == "" {
		s.fail(w, r, 400, "identifier is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.LoadTimeout())
	defer cancel()
	v, e := s.deps.Loader.Load(ctx, id)
	if e != nil {
		v = protocol.LoadError(e)
	}
	s.json(w, 200, v)
}
func (s *Server) decodeTrack(w http.ResponseWriter, r *http.Request) {
	t, e := protocol.Decode(r.URL.Query().Get("encodedTrack"))
	if e != nil {
		s.fail(w, r, 400, e.Error())
		return
	}
	s.json(w, 200, t)
}
func (s *Server) decodeTracks(w http.ResponseWriter, r *http.Request) {
	var encoded []string
	if !s.decode(w, r, &encoded) {
		return
	}
	if len(encoded) > 1000 {
		s.fail(w, r, 400, "at most 1000 tracks per request")
		return
	}
	tracks := make([]*protocol.Track, 0, len(encoded))
	for _, v := range encoded {
		t, e := protocol.Decode(v)
		if e != nil {
			s.fail(w, r, 400, e.Error())
			return
		}
		tracks = append(tracks, t)
	}
	s.json(w, 200, tracks)
}
func (s *Server) getPlayers(w http.ResponseWriter, r *http.Request) {
	v := s.getSession(w, r)
	if v == nil {
		return
	}
	s.json(w, 200, v.snapshots())
}
func (s *Server) getPlayer(w http.ResponseWriter, r *http.Request) {
	v := s.getSession(w, r)
	if v == nil {
		return
	}
	v.mu.Lock()
	p := v.players[r.PathValue("guildId")]
	v.mu.Unlock()
	if p == nil {
		s.fail(w, r, 404, "player not found")
		return
	}
	s.json(w, 200, p.Snapshot())
}
func (s *Server) updatePlayer(w http.ResponseWriter, r *http.Request) {
	v := s.getSession(w, r)
	if v == nil {
		return
	}
	guild := r.PathValue("guildId")
	if !voice.ValidID(guild) {
		s.fail(w, r, 400, "invalid guild ID")
		return
	}
	var u protocol.PlayerUpdate
	if !s.decode(w, r, &u) {
		return
	}
	if e := player.Validate(u); e != nil {
		s.fail(w, r, 400, e.Error())
		return
	}
	if u.Filters != nil {
		// The reference implementation refuses a filter the operator disabled
		// before it touches the player, and names every offending filter.
		if names := audio.DisabledFilters(*u.Filters, s.config.Filters); len(names) > 0 {
			s.fail(w, r, 400, "Following filters are disabled in the config: "+strings.Join(names, ", "))
			return
		}
	}
	noReplace := false
	if raw := r.URL.Query().Get("noReplace"); raw != "" {
		var e error
		noReplace, e = strconv.ParseBool(raw)
		if e != nil {
			s.fail(w, r, 400, "noReplace must be a boolean")
			return
		}
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		s.fail(w, r, 404, "session is closed")
		return
	}
	p := v.players[guild]
	created := false
	if p == nil {
		if s.playerCount.Add(1) > int64(s.config.MaxPlayers) {
			s.playerCount.Add(-1)
			v.mu.Unlock()
			s.fail(w, r, 503, "player capacity reached")
			return
		}
		p = player.New(v.user, guild, s.deps.Engine, s.deps.Loader, s.deps.Voice, v.emit, &s.counters)
		v.players[guild] = p
		created = true
		s.deps.Logger.Info("player created",
			"guildId", guild,
			"sessionId", v.id,
			"players", s.playerCount.Load(),
		)
	}
	v.mu.Unlock()
	if created {
		s.requestReport()
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.LoadTimeout())
	defer cancel()
	result, e := p.Update(ctx, u, noReplace)
	if e != nil {
		// Another PATCH may already be queued on this player. Keep its identity
		// stable even when a source lookup fails before any state is changed.
		s.fail(w, r, 400, e.Error())
		return
	}
	s.json(w, 200, result)
}
func (s *Server) deletePlayer(w http.ResponseWriter, r *http.Request) {
	v := s.getSession(w, r)
	if v == nil {
		return
	}
	v.mu.Lock()
	p := v.players[r.PathValue("guildId")]
	if p != nil {
		delete(v.players, r.PathValue("guildId"))
		s.playerCount.Add(-1)
	}
	v.mu.Unlock()
	if p != nil {
		p.Destroy()
		s.deps.Logger.Info("player destroyed", "guildId", r.PathValue("guildId"), "sessionId", v.id, "players", s.playerCount.Load())
		s.requestReport()
	}
	w.WriteHeader(204)
}
func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	v := s.getSession(w, r)
	if v == nil {
		return
	}
	var update struct {
		Resuming *bool `json:"resuming"`
		Timeout  *int  `json:"timeout"`
	}
	if !s.decode(w, r, &update) {
		return
	}
	if update.Timeout != nil && (*update.Timeout < 0 || *update.Timeout > 86400) {
		s.fail(w, r, 400, "timeout must be 0..86400 seconds")
		return
	}
	v.mu.Lock()
	if !v.connected || v.closed {
		v.mu.Unlock()
		s.fail(w, r, 404, "session not found")
		return
	}
	if update.Resuming != nil {
		v.resuming = *update.Resuming
	}
	if update.Timeout != nil {
		v.timeout = *update.Timeout
	}
	out := protocol.Object{"resuming": v.resuming, "timeout": v.timeout}
	v.mu.Unlock()
	s.json(w, 200, out)
}
func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("User-Id")
	if !voice.ValidID(user) || !strings.Contains(r.Header.Get("Client-Name"), "/") {
		s.fail(w, r, 400, "User-Id and Client-Name (name/version) are required")
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.fail(w, r, 503, "server is shutting down")
		return
	}
	var v *session
	resumed := false
	if id := r.Header.Get("Session-Id"); id != "" {
		v = s.sessions[id]
		if v != nil {
			v.mu.Lock()
			if v.user != user {
				v.mu.Unlock()
				s.mu.Unlock()
				s.fail(w, r, 403, "session belongs to another bot")
				return
			}
			if v.connected || v.attaching {
				v.mu.Unlock()
				s.mu.Unlock()
				s.fail(w, r, 409, "session already connected")
				return
			}
			resumed = v.resuming && !v.closed && time.Since(v.disconnected) < time.Duration(v.timeout)*time.Second
			if resumed {
				v.attaching = true
			}
			v.mu.Unlock()
			if !resumed {
				v = nil
			}
		}
	}
	if v == nil {
		if len(s.sessions) >= s.config.MaxSessions {
			s.mu.Unlock()
			s.fail(w, r, 503, "session capacity reached")
			return
		}
		var b [18]byte
		if _, e := rand.Read(b[:]); e != nil {
			s.mu.Unlock()
			s.fail(w, r, 500, "failed to create session")
			return
		}
		id := hex.EncodeToString(b[:])
		v = &session{id: id, user: user, timeout: 60, attaching: true, players: map[string]*player.Player{}, events: make(chan []byte, s.config.EventBuffer), done: make(chan struct{}), server: s}
		s.sessions[id] = v
	}
	sessionCount := len(s.sessions)
	s.mu.Unlock()
	upgrader := websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, HandshakeTimeout: 10 * time.Second}
	// Lavalink answers the handshake with the API version, the session id and
	// whether the session was resumed.
	headers := http.Header{
		"Lavalink-Api-Version": []string{"4"},
		"Session-Id":           []string{v.id},
		"Session-Resumed":      []string{strconv.FormatBool(resumed)},
	}
	c, e := upgrader.Upgrade(w, r, headers)
	if e != nil {
		if resumed {
			// A failed handshake never attached. Keep the original resume deadline.
			v.mu.Lock()
			v.attaching = false
			v.mu.Unlock()
		} else {
			s.remove(v.id)
		}
		s.deps.Logger.Warn("websocket upgrade failed", "sessionId", v.id, "error", e)
		return
	}
	v.mu.Lock()
	v.attaching = false
	if v.closed {
		v.mu.Unlock()
		c.Close()
		return
	}
	v.conn = c
	v.connected = true
	playerCount := len(v.players)
	v.mu.Unlock()
	defer func() { c.Close(); s.detach(v, c) }()
	if resumed {
		s.deps.Logger.Info("session resumed", "sessionId", v.id, "userId", user, "players", playerCount)
	} else {
		s.deps.Logger.Info("session created", "sessionId", v.id, "userId", user, "client", r.Header.Get("Client-Name"), "sessions", sessionCount)
	}
	c.SetReadLimit(4096)
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if c.WriteJSON(protocol.Object{"op": "ready", "resumed": resumed, "sessionId": v.id}) != nil {
		return
	}
	// A client arriving is one more moment worth reporting, through the same
	// latency as every other one.
	s.requestReport()
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); v.writeLoop(c, stop) }()
	defer func() { close(stop); c.Close(); <-writerDone }()
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}
func (s *Server) detach(v *session, c *websocket.Conn) {
	v.mu.Lock()
	if v.closed || v.conn != c {
		v.mu.Unlock()
		return
	}
	v.conn = nil
	v.connected = false
	v.disconnected = time.Now()
	timeout := v.timeout
	keep := v.resuming && timeout > 0
	players := len(v.players)
	v.mu.Unlock()
	if keep {
		s.deps.Logger.Info("session disconnected, awaiting resume",
			"sessionId", v.id,
			"timeoutSeconds", timeout,
			"players", players,
		)
		return
	}
	s.deps.Logger.Info("session closed", "sessionId", v.id, "players", players)
	s.remove(v.id)
}
func (s *Server) remove(id string) {
	s.mu.Lock()
	v := s.sessions[id]
	delete(s.sessions, id)
	if v != nil {
		s.cleanup.Add(1)
	}
	s.mu.Unlock()
	if v != nil {
		defer s.cleanup.Done()
		v.destroy()
	}
}
func (s *Server) reap() {
	defer s.wg.Done()
	s.cpu.Sample()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.cpu.Sample()
			s.mu.Lock()
			var expired []*session
			for id, v := range s.sessions {
				v.mu.Lock()
				stale := !v.attaching && !v.connected && (!v.resuming || time.Since(v.disconnected) >= time.Duration(v.timeout)*time.Second)
				if stale {
					delete(s.sessions, id)
					expired = append(expired, v)
				}
				v.mu.Unlock()
			}
			s.mu.Unlock()
			for _, v := range expired {
				v.mu.Lock()
				resumable, timeout, players := v.resuming, v.timeout, len(v.players)
				v.mu.Unlock()
				s.deps.Logger.Info("session expired",
					"sessionId", v.id,
					"userId", v.user,
					"resumable", resumable,
					"timeoutSeconds", timeout,
					"players", players,
				)
				v.destroy()
			}
		}
	}
}
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	sessions := s.sessions
	s.sessions = map[string]*session{}
	s.mu.Unlock()
	for _, v := range sessions {
		v.destroy()
	}
	s.wg.Wait()
	s.cleanup.Wait()
}
func (s *Server) stats() protocol.Object {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, v := range s.sessions {
		sessions = append(sessions, v)
	}
	s.mu.Unlock()
	playing := 0
	for _, v := range sessions {
		for _, p := range v.snapshots() {
			if p.Track != nil && !p.Paused && p.State.Connected {
				playing++
			}
		}
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	systemLoad, processLoad, _ := s.cpu.Values()
	return protocol.Object{"players": s.playerCount.Load(), "playingPlayers": playing, "uptime": time.Since(s.started).Milliseconds(), "memory": protocol.Object{"free": mem.HeapIdle, "used": mem.HeapAlloc, "allocated": mem.Sys, "reservable": mem.HeapSys}, "cpu": protocol.Object{"cores": runtime.NumCPU(), "systemLoad": systemLoad, "lavalinkLoad": processLoad}}
}

// requestReport asks for a fresh report of every player. It never blocks, and a
// burst of activity collapses into the one report the latency allows.
func (s *Server) requestReport() {
	if s.deps.Webhook == nil {
		return
	}
	select {
	case s.reports <- struct{}{}:
	default:
	}
}

// reportLoop sends the reports the node asks for, starting with one at start up
// so a restart shows in the channel before anything is playing.
func (s *Server) reportLoop() {
	defer s.wg.Done()
	s.requestReport()
	for {
		select {
		case <-s.done:
			return
		case <-s.reports:
			s.reportPlayerState()
		}
	}
}

// reportPlayerState posts the node-wide summary of every player, which is what
// an operator watches; a client only ever sees its own playerUpdate. Reports are
// spaced by stateLatencySeconds: one asked for sooner than that after the last
// is cancelled rather than queued, so a busy node posts on that interval and an
// idle one goes quiet.
func (s *Server) reportPlayerState() {
	latency := s.config.Webhook.StateLatencySeconds
	if s.deps.Webhook == nil || latency < 1 {
		return
	}

	s.reportMu.Lock()
	if time.Since(s.lastReport) < time.Duration(latency)*time.Second {
		s.reportMu.Unlock()
		return
	}
	s.lastReport = time.Now()
	s.reportMu.Unlock()

	report, _ := s.playerStateReport()

	s.deps.Webhook.Send(report)
}

// playerStateReport summarises every player on the node. It is meant for a
// channel other people may read, so it carries only what describes playback:
// the session and user that own a player are left out, along with the encoded
// track and every other field a reader has no business seeing.
func (s *Server) playerStateReport() (protocol.Object, int) {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, v := range s.sessions {
		sessions = append(sessions, v)
	}
	s.mu.Unlock()
	states := make([]protocol.Object, 0, int(s.playerCount.Load()))
	playing := 0
	for _, v := range sessions {
		v.mu.Lock()
		players := make([]*player.Player, 0, len(v.players))
		for _, p := range v.players {
			players = append(players, p)
		}
		v.mu.Unlock()
		for _, p := range players {
			snapshot := p.Snapshot()
			if snapshot.Track != nil && !snapshot.Paused && snapshot.State.Connected {
				playing++
			}
			var info protocol.TrackInfo
			if snapshot.Track != nil {
				info = snapshot.Track.Info
			}
			states = append(states, protocol.Object{
				"guildId":   snapshot.GuildID,
				"title":     info.Title,
				"author":    info.Author,
				"source":    info.SourceName,
				"length":    info.Length,
				"position":  snapshot.State.Position,
				"endpoint":  snapshot.Voice.Endpoint,
				"volume":    snapshot.Volume,
				"paused":    snapshot.Paused,
				"connected": snapshot.State.Connected,
				"ping":      snapshot.State.Ping,
			})
		}
	}
	return protocol.Object{
		"type":           webhook.PlayerStateType,
		"timestamp":      time.Now().UnixMilli(),
		"sessions":       len(sessions),
		"players":        len(states),
		"playingPlayers": playing,
		"states":         states,
	}, len(states)
}
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	// Sources are reported from what is actually usable: the native clients do
	// not need yt-dlp, so its absence no longer hides YouTube or SoundCloud.
	sources := []string{"http"}
	if s.config.YoutubeNative {
		sources = append(sources, "youtube")
	}
	if s.config.SoundCloudNative {
		sources = append(sources, "soundcloud")
	}
	// YTDLPEnabled is only left standing once startup has resolved a runnable
	// yt-dlp, so the flag itself is the answer here.
	if s.config.YTDLPEnabled {
		if !s.config.YoutubeNative {
			sources = append(sources, "youtube")
		}
		if !s.config.SoundCloudNative {
			sources = append(sources, "soundcloud")
		}
		// yt-dlp also backs the "youtube" search prefixes and Bandcamp and
		// similar generic extractors, which have no native client here.
		sources = append(sources, "yt-dlp")
	}
	if s.config.SpotifyClientID != "" {
		sources = append(sources, "spotify")
	}
	sources = append(sources, "deezer")
	_, _, cpuAvailable := s.cpu.Values()
	s.json(w, 200, protocol.Object{"version": protocol.Object{"semver": APIVersion + "+golalink." + Version, "major": 4, "minor": 2, "patch": 2, "preRelease": "", "build": "golalink." + Version}, "buildTime": int64(0), "git": protocol.Object{"branch": "unknown", "commit": "unknown", "commitTime": int64(0)}, "jvm": runtime.Version(), "lavaplayer": "ffmpeg", "sourceManagers": sources, "filters": audio.FilterNames, "plugins": []any{}, "golalink": protocol.Object{"version": Version, "apiCompatibility": "4.2", "dave": "dave-go", "cpuStatsAvailable": cpuAvailable}})
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s.mu.Lock()
	sessions := len(s.sessions)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, m := range []struct {
		name, kind, help string
		value            any
	}{{"golalink_players", "gauge", "Current players", s.playerCount.Load()}, {"golalink_sessions", "gauge", "Current sessions", sessions}, {"golalink_http_requests_total", "counter", "HTTP requests", s.requests.Load()}, {"golalink_audio_frames_total", "counter", "Opus frames sent", s.counters.Sent.Load()}, {"golalink_audio_deficit_total", "counter", "Missed frame deadlines", s.counters.Deficit.Load()}, {"golalink_audio_errors_total", "counter", "Playback failures", s.counters.Errors.Load()}, {"golalink_heap_bytes", "gauge", "Go heap bytes; excludes media subprocesses", mem.HeapAlloc}, {"golalink_goroutines", "gauge", "Go goroutines", runtime.NumGoroutine()}, {"golalink_uptime_seconds", "gauge", "Uptime in seconds", time.Since(s.started).Seconds()}} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", m.name, m.help, m.name, m.kind, m.name, m.value)
	}
}
