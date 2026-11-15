package source

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golalink/internal/config"
	"golalink/internal/netguard"
	"golalink/internal/process"
	"golalink/internal/protocol"
	"golang.org/x/sync/singleflight"
)

type cacheEntry struct {
	key     string
	data    []byte
	expires time.Time
}
type Service struct {
	Config     config.Config
	HTTP       *http.Client
	Spotify    *Spotify
	Deezer     *Deezer
	SoundCloud *SoundCloud
	// Logger reports which resolution path served a load. It may be left nil,
	// in which case the default logger is used.
	Logger     *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc
	loads      chan struct{}
	flight     singleflight.Group
	mu         sync.Mutex
	cache      map[string]*list.Element
	lru        *list.List
	cacheBytes int
	CacheHits  atomic.Uint64
	LoadCount  atomic.Uint64
}

func New(c config.Config) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	h := netguard.NewClient(c.AllowPrivateNetworks)
	return &Service{Config: c, HTTP: h, Spotify: &Spotify{client: h, clientID: c.SpotifyClientID, secret: c.SpotifyClientSecret, refreshToken: c.SpotifyRefreshToken, market: c.SpotifyMarket, baseURL: "https://api.spotify.com/v1", tokenURL: "https://accounts.spotify.com/api/token"}, Deezer: &Deezer{client: h, baseURL: "https://api.deezer.com"}, SoundCloud: &SoundCloud{client: h, apiBaseURL: c.SoundCloudAPIEndpoint, webBaseURL: c.SoundCloudWebEndpoint, override: c.SoundCloudClientID, ttl: c.SoundCloudClientIDTTL()}, ctx: ctx, cancel: cancel, loads: make(chan struct{}, c.MaxConcurrentLoads), cache: map[string]*list.Element{}, lru: list.New()}
}
func (s *Service) Close() { s.cancel(); s.HTTP.CloseIdleConnections() }

// log returns the configured logger, falling back to the process default so a
// Service built directly in a test still works.
func (s *Service) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
func (s *Service) Load(ctx context.Context, identifier string) (protocol.LoadResult, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" || len(identifier) > 8192 {
		return protocol.LoadResult{}, errors.New("identifier must contain 1..8192 bytes")
	}
	s.mu.Lock()
	e := s.cache[identifier]
	if e != nil {
		v := e.Value.(cacheEntry)
		if time.Now().Before(v.expires) {
			s.lru.MoveToFront(e)
			s.mu.Unlock()
			s.CacheHits.Add(1)
			s.log().Debug("load cache hit", "identifier", identifier, "bytes", len(v.data))
			return decodeResult(v.data)
		}
		s.lru.Remove(e)
		s.cacheBytes -= len(v.data)
		delete(s.cache, identifier)
	}
	s.mu.Unlock()
	ch := s.flight.DoChan(identifier, func() (any, error) {
		work, cancel := context.WithTimeout(s.ctx, s.Config.LoadTimeout())
		defer cancel()
		select {
		case s.loads <- struct{}{}:
			defer func() { <-s.loads }()
		case <-work.Done():
			return nil, work.Err()
		default:
			return nil, errors.New("source resolver capacity reached; retry shortly")
		}
		s.LoadCount.Add(1)
		started := time.Now()
		result, err := s.load(work, identifier)
		elapsed := time.Since(started)
		if err != nil {
			s.log().Warn("load failed",
				"identifier", identifier,
				"source", sourceKind(identifier),
				"durationMs", elapsed.Milliseconds(),
				"error", err,
			)
			return nil, err
		}
		s.log().Debug("load resolved",
			"identifier", identifier,
			"source", sourceKind(identifier),
			"loadType", result.LoadType,
			"durationMs", elapsed.Milliseconds(),
		)
		b, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if result.LoadType != "error" && len(b) <= 2<<20 {
			s.mu.Lock()
			if old := s.cache[identifier]; old != nil {
				s.cacheBytes -= len(old.Value.(cacheEntry).data)
				s.lru.Remove(old)
			}
			s.cache[identifier] = s.lru.PushFront(cacheEntry{identifier, b, time.Now().Add(time.Duration(s.Config.CacheTTLSeconds) * time.Second)})
			s.cacheBytes += len(b)
			for s.lru.Len() > s.Config.CacheEntries || s.cacheBytes > 32<<20 {
				last := s.lru.Back()
				s.cacheBytes -= len(last.Value.(cacheEntry).data)
				delete(s.cache, last.Value.(cacheEntry).key)
				s.lru.Remove(last)
			}
			s.mu.Unlock()
		}
		return b, nil
	})
	select {
	case <-ctx.Done():
		return protocol.LoadResult{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return protocol.LoadResult{}, res.Err
		}
		return decodeResult(res.Val.([]byte))
	}
}
func decodeResult(b []byte) (protocol.LoadResult, error) {
	var raw struct {
		LoadType string          `json:"loadType"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return protocol.LoadResult{}, err
	}
	r := protocol.LoadResult{LoadType: raw.LoadType}
	switch raw.LoadType {
	case "track":
		var t protocol.Track
		if err := json.Unmarshal(raw.Data, &t); err != nil {
			return r, err
		}
		r.Data = &t
	case "search":
		var ts []*protocol.Track
		if err := json.Unmarshal(raw.Data, &ts); err != nil {
			return r, err
		}
		r.Data = ts
	case "playlist":
		var p protocol.Playlist
		if err := json.Unmarshal(raw.Data, &p); err != nil {
			return r, err
		}
		r.Data = p
	default:
		if err := json.Unmarshal(raw.Data, &r.Data); err != nil {
			return r, err
		}
	}
	return r, nil
}

var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func sourceKind(id string) string {
	if strings.HasPrefix(id, "spsearch:") || strings.HasPrefix(id, "spotify:") {
		return "spotify"
	}
	if strings.HasPrefix(id, "dzsearch:") || strings.HasPrefix(id, "dzisrc:") || strings.HasPrefix(id, "deezer:") {
		return "deezer"
	}
	if strings.HasPrefix(id, "ytsearch:") || strings.HasPrefix(id, "ytmsearch:") || youtubeID.MatchString(id) {
		return "youtube"
	}
	if strings.HasPrefix(id, "scsearch:") {
		return "soundcloud"
	}
	u, err := url.Parse(id)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	for domain, name := range map[string]string{"youtube.com": "youtube", "youtu.be": "youtube", "soundcloud.com": "soundcloud", "open.spotify.com": "spotify", "deezer.com": "deezer"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return name
		}
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return "http"
	}
	return ""
}
func (s *Service) load(ctx context.Context, id string) (protocol.LoadResult, error) {
	switch sourceKind(id) {
	case "spotify":
		return s.Spotify.Load(ctx, id, s.Config.SearchResults, s.Config.MaxPlaylistTracks)
	case "deezer":
		return s.Deezer.Load(ctx, id, s.Config.SearchResults, s.Config.MaxPlaylistTracks)
	case "youtube":
		return s.loadYouTube(ctx, id)
	case "soundcloud":
		return s.loadSoundCloud(ctx, id)
	case "http":
		return s.loadHTTP(ctx, id)
	default:
		return protocol.LoadResult{LoadType: "empty", Data: nil}, nil
	}
}

// loadYouTube prefers the native InnerTube paths and keeps yt-dlp as the
// fallback, so a client-version regression degrades instead of breaking.
// Both reasons are reported when neither path works.
func (s *Service) loadYouTube(ctx context.Context, id string) (protocol.LoadResult, error) {
	if !s.Config.YoutubeNative {
		s.log().Debug("youtube resolution uses the yt-dlp path (native disabled)", "identifier", id)
		return s.loadYTDLP(ctx, id)
	}
	started := time.Now()
	native, nativeErr := s.loadYouTubeNative(ctx, id)
	if nativeErr == nil {
		s.log().Debug("youtube resolved natively", "identifier", id, "loadType", native.LoadType, "durationMs", time.Since(started).Milliseconds())
		return native, nil
	}
	// debug rather than warn: falling back is expected when a client version
	// ages out, and the combined error below is what an operator needs.
	s.log().Debug(s.ytdlpFallbackMessage("youtube"),
		"identifier", id,
		"durationMs", time.Since(started).Milliseconds(),
		"error", nativeErr,
	)
	fallback, err := s.loadYTDLP(ctx, id)
	if err == nil {
		return fallback, nil
	}
	return protocol.LoadResult{}, fmt.Errorf("native youtube resolution failed (%v); yt-dlp fallback failed (%v)", nativeErr, err)
}

// ytdlpFallbackMessage describes the fallback that is about to be attempted, or
// states that it is switched off. A log line must never promise a fallback that
// cannot happen, which is what an operator sees with ytDlpEnabled: false.
func (s *Service) ytdlpFallbackMessage(source string) string {
	if s.Config.YTDLPEnabled {
		return "native " + source + " resolution failed; falling back to yt-dlp"
	}
	return "native " + source + " resolution failed; the yt-dlp fallback is disabled"
}

// mediaError carries the status of a rejected media request so callers can tell
// a stale or throttled URL from a permanent failure.
type mediaError struct{ Status int }

func (e *mediaError) Error() string { return fmt.Sprintf("media server returned HTTP %d", e.Status) }

// retryable reports whether resolving the track again may yield a usable URL.
// googlevideo rejects a share of otherwise valid URLs with 403, and an expired
// URL surfaces as 404 or 410; both are cured by resolving a fresh one.
func (e *mediaError) retryable() bool {
	switch e.Status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests:
		return true
	}
	return e.Status >= 500
}

func retryableMedia(err error) bool {
	var m *mediaError
	return errors.As(err, &m) && m.retryable()
}

// fetch returns the body and the total length the server declared, which is -1
// when it did not. The length refines the duration of a few formats.
func (s *Service) fetch(ctx context.Context, raw string, headers map[string]string) (io.ReadCloser, int64, error) {
	media, err := s.fetchMedia(ctx, raw, headers, false, 0)
	return media.body, media.size, err
}

type mediaResponse struct {
	body    io.ReadCloser
	size    int64 // total length, or -1 if unknown
	end     int64 // absolute end of this response, exclusive; -1 if unknown
	partial bool
}

// Playback requests a byte range because some CDNs reject plain GETs. A 200
// response ignores the requested offset; the caller can discard the prefix.
func (s *Service) fetchMedia(ctx context.Context, raw string, headers map[string]string, ranged bool, offset int64) (mediaResponse, error) {
	if err := netguard.ValidateURL(raw); err != nil {
		return mediaResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return mediaResponse{}, err
	}
	req.Header.Set("User-Agent", "Golalink/0.1")
	for k, v := range headers {
		if !strings.ContainsAny(k+v, "\r\n") {
			req.Header.Set(k, v)
		}
	}
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("Accept-Encoding", "identity")
	}
	r, err := s.HTTP.Do(req)
	if err != nil {
		return mediaResponse{}, err
	}
	contentRange := r.Header.Get("Content-Range")
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		r.Body.Close()
		// When earlier ranges did not report a total, a 416 at exactly the
		// reported length is how the server signals the end of the file.
		if ranged && offset > 0 && r.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			unit, value, _ := strings.Cut(contentRange, " ")
			if total, ok := strings.CutPrefix(value, "*/"); strings.EqualFold(unit, "bytes") && ok {
				if size, err := strconv.ParseInt(total, 10, 64); err == nil && size == offset {
					return mediaResponse{body: emptyBody{}, size: size, end: size, partial: true}, nil
				}
			}
		}
		return mediaResponse{}, &mediaError{Status: r.StatusCode}
	}
	if r.StatusCode != http.StatusPartialContent {
		return mediaResponse{body: r.Body, size: r.ContentLength, end: r.ContentLength}, nil
	}
	start, end, size, err := parseMediaRange(contentRange)
	if err != nil || start != offset || (r.ContentLength >= 0 && r.ContentLength != end-start) {
		r.Body.Close()
		return mediaResponse{}, fmt.Errorf("invalid Content-Range %q for media offset %d", contentRange, offset)
	}
	return mediaResponse{body: r.Body, size: size, end: end, partial: true}, nil
}

func parseMediaRange(value string) (start, end, size int64, err error) {
	unit, rest, _ := strings.Cut(value, " ")
	span, total, _ := strings.Cut(rest, "/")
	first, last, _ := strings.Cut(span, "-")
	lo, loErr := strconv.ParseInt(first, 10, 64)
	hi, hiErr := strconv.ParseInt(last, 10, 64)
	if !strings.EqualFold(unit, "bytes") || loErr != nil || hiErr != nil || lo < 0 || hi < lo || hi == math.MaxInt64 {
		return 0, 0, 0, errors.New("invalid media byte range")
	}
	size = -1
	if total != "*" {
		size, err = strconv.ParseInt(total, 10, 64)
		if err != nil || size <= hi {
			return 0, 0, 0, errors.New("invalid media length")
		}
	}
	return lo, hi + 1, size, nil
}
func (s *Service) loadHTTP(ctx context.Context, id string) (protocol.LoadResult, error) {
	body, size, err := s.fetch(ctx, id, nil)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	defer body.Close()
	// The container is read here rather than handed to a separate probe process:
	// every format worth linking to states its duration and tags in the first
	// few KiB, and a live stream is then identified without waiting for audio.
	p, err := ProbeContainer(body, size)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	if !p.Audio {
		return protocol.LoadResult{}, errNoAudio
	}
	u, _ := url.Parse(id)
	title := p.Title
	if title == "" {
		title = path.Base(u.Path)
	}
	author := p.Artist
	if author == "" {
		author = "Unknown artist"
	}
	length := p.Duration.Milliseconds()
	t, err := protocol.Encode(protocol.TrackInfo{Identifier: id, Title: title, Author: author, Length: length, IsStream: length <= 0, IsSeekable: length > 0, URI: &id, SourceName: "http"})
	return protocol.LoadResult{LoadType: "track", Data: t}, err
}

// Open resolves expiring stream URLs only when playback starts, never into track encodings.
func (s *Service) Open(ctx context.Context, t *protocol.Track) (io.ReadCloser, error) {
	if t.Info.SourceName == "http" {
		if t.Info.URI == nil {
			return nil, errors.New("track URI is missing")
		}
		body, err := s.openSeekable(ctx, *t.Info.URI, nil)
		if err != nil {
			return nil, err
		}
		return body, nil
	}
	// Spotify and Deezer supply metadata only; the audio is mirrored from a
	// YouTube search for the same title and artist.
	if t.Info.SourceName == "spotify" || t.Info.SourceName == "deezer" {
		name := "Spotify"
		if t.Info.SourceName == "deezer" {
			name = "Deezer"
		}
		r, err := s.Load(ctx, "ytsearch:"+t.Info.Author+" - "+t.Info.Title+" audio")
		if err != nil {
			return nil, err
		}
		candidates, ok := r.Data.([]*protocol.Track)
		if !ok || len(candidates) == 0 {
			return nil, fmt.Errorf("no playable match for %s track", name)
		}
		best := matchTrack(t, candidates)
		if best == nil {
			return nil, fmt.Errorf("no sufficiently close title, artist and duration match for %s track", name)
		}
		return s.Open(ctx, best)
	}
	if t.Info.SourceName == "soundcloud" && s.Config.SoundCloudNative {
		return s.openSoundCloud(ctx, t)
	}
	if t.Info.SourceName != "youtube" && t.Info.SourceName != "soundcloud" {
		return nil, errors.New("unsupported track source")
	}
	id := t.Info.Identifier
	if t.Info.URI != nil {
		id = *t.Info.URI
	}
	if sourceKind(id) != t.Info.SourceName {
		return nil, errors.New("track URI does not match its source")
	}
	work, cancel := context.WithTimeout(ctx, s.Config.LoadTimeout())
	defer cancel()
	select {
	case s.loads <- struct{}{}:
		defer func() { <-s.loads }()
	case <-work.Done():
		return nil, work.Err()
	}
	return s.openMedia(ctx, work, func(resolveCtx context.Context) (string, map[string]string, error) {
		if s.Config.YoutubeNative && t.Info.SourceName == "youtube" {
			if videoID := itVideoID(t.Info.Identifier); videoID != "" {
				if raw, headers, err := s.itMediaURL(resolveCtx, videoID); err == nil {
					return raw, headers, nil
				}
			}
		}
		return s.ytdlpMediaURL(resolveCtx, id)
	})
}

// ytdlpMediaURL asks yt-dlp for a direct stream URL of an already loaded track.
func (s *Service) ytdlpMediaURL(ctx context.Context, id string) (string, map[string]string, error) {
	if !s.Config.YTDLPEnabled {
		return "", nil, errors.New("yt-dlp is disabled (ytDlpEnabled: false)")
	}
	b, err := process.Run(ctx, s.Config.YTDLP, nil, 4<<20, append(s.ytdlpArgs(), "--no-playlist", "--format", "bestaudio[protocol=https]/bestaudio[protocol=http]", "--dump-single-json", "--", normalizeID(id, s.Config.SearchResults))...)
	if err != nil {
		return "", nil, err
	}
	var m ytMedia
	if err = json.Unmarshal(b, &m); err != nil {
		return "", nil, err
	}
	if m.URL == "" {
		return "", nil, errors.New("source did not return a direct audio stream")
	}
	return m.URL, m.HTTPHeaders, nil
}

// mediaAttempts bounds how often a rejected media URL is replaced by a fresh
// one. Resolution costs a subprocess, so it only runs again after a rejection.
const mediaAttempts = 3

// mediaRetryDelay keeps a retry from hammering the source in a tight loop.
const mediaRetryDelay = 200 * time.Millisecond

// openMedia resolves a media URL and opens it, resolving again when the source
// rejects the URL it handed out. Resolution runs under work so a slow source
// cannot outlive the load timeout, while the returned body stays bound to ctx,
// which the caller keeps alive for the whole playback: cancelling the body at
// the end of the load would leave the decoder with an empty stream.
func (s *Service) openMedia(ctx context.Context, work context.Context, resolve func(context.Context) (string, map[string]string, error)) (io.ReadCloser, error) {
	var last error
	for attempt := 0; attempt < mediaAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(mediaRetryDelay):
			}
		}
		raw, headers, err := resolve(work)
		if err != nil {
			return nil, err
		}
		body, err := s.openSeekable(ctx, raw, headers)
		if err == nil {
			// A dropped connection must not end a track that is already playing,
			// and a container whose header sits at the end needs the length, so
			// the body is wrapped rather than handed on as it came.
			return body, nil
		}
		last = err
		if !retryableMedia(err) {
			return nil, err
		}
	}
	return nil, last
}

// openSeekable opens a playback body wrapped so that it can be read out of
// order. The length is part of that: a seek from the end is how a container with
// its header in the tail is reached.
func (s *Service) openSeekable(ctx context.Context, raw string, headers map[string]string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	media, err := s.fetchMedia(ctx, raw, headers, true, 0)
	if err != nil {
		cancel()
		return nil, err
	}
	return &resumableBody{ctx: ctx, cancel: cancel, svc: s, raw: raw, headers: headers, media: media}, nil
}
