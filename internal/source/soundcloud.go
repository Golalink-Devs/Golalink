package source

// Native SoundCloud resolution through the public api-v2 endpoints, so the
// common paths do not need the external yt-dlp process.
//
// Nothing here needs an account or an API key. SoundCloud authorises these
// requests with a client id that its own web player ships inside its JavaScript
// bundles, so the id is scraped from the page and cached. Config pins one with
// SoundCloudClientID, and Config.SoundCloudClientIDTTL bounds how long a
// scraped id is reused, because SoundCloud rotates them.
//
// Verified against the live service on 2026-10-07:
//
//   - https://soundcloud.com/ serves nine a-v2.sndcdn.com/assets/*.js bundles;
//     the client_id literal was in the bundle loaded last, not in the page.
//   - /search/tracks, /resolve and /tracks/{id} answer without credentials. An
//     unknown client id is refused with HTTP 401.
//   - /resolve answers a track URL with kind "track" and a set URL with kind
//     "playlist", carrying its title, its user.username and its tracks.
//   - A transcoding whose format.protocol is "progressive" resolves to a
//     cf-media.sndcdn.com URL that answers HTTP 200 with Content-Type
//     audio/mpeg, so FFmpeg reads it directly and no HLS demuxing is needed.
//     The same response carried three "hls" transcodings, one of which shares
//     its UUID with the progressive entry.
//   - A set's resolve response lists every entry, but only the first few carry
//     a title and a duration; the rest arrive with an empty title and are
//     skipped. A track whose policy is BLOCK answers /tracks/{id} with an empty
//     transcodings list, so it is not streamable from this node at all, while a
//     policy of SNIP resolves to a cf-preview-media.sndcdn.com URL and reports
//     a 30 second duration, which matches the preview that then plays.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golalink/internal/protocol"
)

// scMaxBody bounds one response. A large set carries a full track object for
// every entry, and the largest asset bundle is a few megabytes.
const scMaxBody = 8 << 20

// scDefaultClientIDTTL covers a config that was built without going through
// config.Default, so a missing TTL cannot turn every load into a scrape.
const scDefaultClientIDTTL = 6 * time.Hour

// scUserAgent is what the native source identifies itself as. SoundCloud served
// the page and the api-v2 endpoints to it when the checks above were made.
const scUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// scScript matches the asset bundles the page loads, as in
// <script crossorigin src="https://a-v2.sndcdn.com/assets/55-76d152dd.js">.
// The path shape identifies a bundle, so a mirror on another host works too.
var scScript = regexp.MustCompile(`<script[^>]+src=["'](https?://[^"']+/assets/[^"']+\.js)["']`)

// scClientID matches the literal the bundles carry, as in client_id:"...".
var scClientID = regexp.MustCompile(`client_id\s*[:=]\s*"([0-9a-zA-Z]{32})"`)

// scNumericID matches the track id used in api-v2 paths.
var scNumericID = regexp.MustCompile(`^[0-9]{1,20}$`)

// SoundCloud is the native SoundCloud source.
type SoundCloud struct {
	client     *http.Client
	apiBaseURL string
	webBaseURL string

	// override pins the client id; when it is empty an id is scraped. ttl
	// bounds how long a scraped id is reused.
	override string
	ttl      time.Duration

	mu        sync.Mutex
	cached    string
	fetchedAt time.Time
}

// scUser is the artist part of a track object.
type scUser struct {
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// scTranscoding is one media.transcodings entry. The protocol and the mime type
// sit under format rather than on the entry itself, which is why a selection by
// a top-level field would never match.
type scTranscoding struct {
	URL    string `json:"url"`
	Preset string `json:"preset"`
	Format struct {
		Protocol string `json:"protocol"`
		MimeType string `json:"mime_type"`
	} `json:"format"`
}

// scTrack is one track object. A playlist object carries its own title and a
// tracks list, so the one shape decodes both and kind tells them apart.
type scTrack struct {
	Kind       string `json:"kind"`
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Duration   int64  `json:"duration"`
	Permalink  string `json:"permalink_url"`
	Artwork    string `json:"artwork_url"`
	Streamable bool   `json:"streamable"`
	Policy     string `json:"policy"`
	User       scUser `json:"user"`
	Publisher  struct {
		ISRC string `json:"isrc"`
	} `json:"publisher_metadata"`
	Media struct {
		Transcodings []scTranscoding `json:"transcodings"`
	} `json:"media"`
	// Tracks is filled only on a playlist object.
	Tracks []scTrack `json:"tracks"`
}

// scRejectedError marks a request refused for its client id, the only refusal a
// freshly scraped id can cure.
type scRejectedError struct{ Status int }

func (e *scRejectedError) Error() string {
	return fmt.Sprintf("SoundCloud rejected the client id with HTTP %d", e.Status)
}

func scRejected(err error) bool {
	var e *scRejectedError
	return errors.As(err, &e)
}

// clientID returns the client id, scraping and caching one when no manual
// override is configured. The lock is held across the scrape so concurrent
// loads share one fetch instead of racing each other.
func (c *SoundCloud) clientID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.override != "" {
		return c.override, nil
	}
	ttl := c.ttl
	if ttl <= 0 {
		ttl = scDefaultClientIDTTL
	}
	if c.cached != "" && time.Since(c.fetchedAt) < ttl {
		return c.cached, nil
	}
	id, err := c.scrape(ctx)
	if err != nil {
		return "", err
	}
	c.cached, c.fetchedAt = id, time.Now()
	return id, nil
}

// forget drops a cached id the API refused, so the next call scrapes a fresh
// one. A manual override is never dropped: it was not scraped, so there is
// nothing fresher to find, and only the configuration can replace it.
func (c *SoundCloud) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == id {
		c.cached, c.fetchedAt = "", time.Time{}
	}
}

// scrape reads the web page, collects its asset bundles and searches them from
// the last one backwards, because the page loads the bundle carrying the client
// id last. Every bundle is tried, so an id that moves to an earlier one is
// still found.
//
// Bundles are megabytes wide and the id sits in the first kilobyte of one of
// them, so each one is asked for its head with a range request first. Measured
// on 2026-10-07: the bundle carrying the id is 2.9 MB and takes 61s in full,
// while its first 64 KB arrive in 0.4s with the id at byte 975. A bundle whose
// head carries nothing is read in full, so a moved id is still found.
func (c *SoundCloud) scrape(ctx context.Context) (string, error) {
	base := strings.TrimSuffix(c.webBaseURL, "/")
	assets, seen := []string{}, map[string]bool{}
	var last error
	// The page occasionally answers with a small JavaScript stub instead of the
	// player, which carries no bundles; the discover page then still does.
	for _, page := range []string{base + "/", base + "/discover"} {
		body, err := c.fetchText(ctx, page)
		if err != nil {
			last = err
			continue
		}
		for _, m := range scScript.FindAllStringSubmatch(body, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				assets = append(assets, m[1])
			}
		}
		if len(assets) > 0 {
			break
		}
	}
	if len(assets) == 0 {
		if last != nil {
			return "", fmt.Errorf("the SoundCloud page carried no asset bundle to read a client id from (%v)", last)
		}
		return "", errors.New("the SoundCloud page carried no asset bundle to read a client id from")
	}
	var lastErr error
	for i := len(assets) - 1; i >= 0; i-- {
		head, complete, err := c.fetchTextRange(ctx, assets[i], scBundleHead)
		if err != nil {
			lastErr = err
			continue
		}
		if m := scClientID.FindStringSubmatch(head); m != nil {
			return m[1], nil
		}
		if complete {
			// The probe already covered the whole bundle.
			continue
		}
		bundle, err := c.fetchText(ctx, assets[i])
		if err != nil {
			lastErr = err
			continue
		}
		if m := scClientID.FindStringSubmatch(bundle); m != nil {
			return m[1], nil
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("no client id in the SoundCloud assets (%v)", lastErr)
	}
	return "", errors.New("no client id in the SoundCloud assets")
}

// scBundleHead is how much of a bundle the ranged probe asks for. It is large
// enough to cover where the id has been observed and small enough to stay fast.
const scBundleHead = 64 << 10

// fetchText reads one web page or bundle. Redirects are followed by the shared
// client, which revalidates every hop.
func (c *SoundCloud) fetchText(ctx context.Context, raw string) (string, error) {
	body, _, err := c.fetchTextRange(ctx, raw, 0)
	return body, err
}

// fetchTextRange reads at most limit bytes and reports whether that covered the
// whole body; limit 0 reads all of it. A server that ignores the range simply
// returns everything, which counts as complete and is still correct.
func (c *SoundCloud) fetchTextRange(ctx context.Context, raw string, limit int64) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "text/html,application/javascript,*/*")
	req.Header.Set("User-Agent", scUserAgent)
	if limit > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", limit-1))
	}
	r, err := c.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return "", false, fmt.Errorf("SoundCloud returned HTTP %d for %s", r.StatusCode, raw)
	}
	body := io.Reader(r.Body)
	if limit > 0 {
		body = io.LimitReader(r.Body, limit)
	} else {
		body = io.LimitReader(r.Body, scMaxBody)
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return "", false, err
	}
	complete := true
	if limit > 0 {
		// A 200 to a ranged request, or a short body, means everything arrived.
		complete = r.StatusCode == http.StatusOK || int64(len(b)) < limit
	}
	return string(b), complete, nil
}

// api builds an api-v2 URL. The client id is added by fetch.
func (c *SoundCloud) api(path string, query url.Values) string {
	raw := strings.TrimSuffix(c.apiBaseURL, "/") + path
	if len(query) > 0 {
		raw += "?" + query.Encode()
	}
	return raw
}

// fetch reads one api-v2 URL with the client id attached. SoundCloud rotates
// the id, so a 401 or 403 drops the cached one and the request is tried once
// more with a freshly scraped id; it is never tried a third time.
func (c *SoundCloud) fetch(ctx context.Context, raw string, dest any) error {
	id, err := c.clientID(ctx)
	if err != nil {
		return err
	}
	err = c.fetchWithID(ctx, raw, id, dest)
	if !scRejected(err) {
		return err
	}
	c.forget(id)
	fresh, freshErr := c.clientID(ctx)
	if freshErr != nil {
		return fmt.Errorf("SoundCloud rejected the client id (%v); scraping a fresh one failed (%v)", err, freshErr)
	}
	if fresh == id {
		// A pinned id cannot be refreshed, so repeating the request would send
		// the same id and get the same refusal.
		return err
	}
	return c.fetchWithID(ctx, raw, fresh, dest)
}

func (c *SoundCloud) fetchWithID(ctx context.Context, raw, id string, dest any) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("client_id", id)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", scUserAgent)
	r, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	switch {
	case r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden:
		return &scRejectedError{Status: r.StatusCode}
	case r.StatusCode == http.StatusNotFound:
		return errors.New("SoundCloud resource not found")
	case r.StatusCode < 200 || r.StatusCode >= 300:
		return fmt.Errorf("SoundCloud returned HTTP %d", r.StatusCode)
	}
	if dest == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(r.Body, scMaxBody)).Decode(dest)
}

// scTrackInfo turns one track object into Lavalink track info. SoundCloud
// reports durations in milliseconds, which is already the unit track info uses.
func scTrackInfo(t scTrack) (protocol.TrackInfo, error) {
	if t.ID == 0 || strings.TrimSpace(t.Title) == "" {
		return protocol.TrackInfo{}, errors.New("SoundCloud returned an incomplete track")
	}
	author := strings.TrimSpace(t.User.Username)
	if author == "" {
		author = "Unknown artist"
	}
	info := protocol.TrackInfo{
		Identifier: strconv.FormatInt(t.ID, 10),
		Title:      t.Title,
		Author:     author,
		Length:     t.Duration,
		IsSeekable: true,
		URI:        protocol.String(t.Permalink),
		SourceName: "soundcloud",
	}
	// artwork_url is null on many uploads, where the artist avatar is the only
	// image SoundCloud offers.
	art := t.Artwork
	if art == "" {
		art = t.User.AvatarURL
	}
	if art != "" {
		info.ArtworkURL = protocol.String(art)
	}
	if t.Publisher.ISRC != "" {
		info.ISRC = protocol.String(t.Publisher.ISRC)
	}
	return info, nil
}

func scEncode(t scTrack) (*protocol.Track, error) {
	info, err := scTrackInfo(t)
	if err != nil {
		return nil, err
	}
	return protocol.Encode(info)
}

// scRef normalises a SoundCloud link. api-v2 decides whether a link is a track
// or a set, so only the host is checked here and /resolve reports the kind.
func scRef(id string) (string, error) {
	raw := strings.TrimSpace(id)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid SoundCloud URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "soundcloud.com" && !strings.HasSuffix(host, ".soundcloud.com") {
		return "", errors.New("invalid SoundCloud URL")
	}
	// m.soundcloud.com and a country path both resolve against the canonical
	// host, and tracking parameters are not part of the resource.
	u.Scheme, u.Host = "https", "soundcloud.com"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// scProgressive picks the progressive transcoding, which resolves to a plain
// HTTP MP3. The live responses carry up to three hls entries beside it, and an
// hls entry is not used here because it would need an HLS demuxer path.
func scProgressive(list []scTranscoding) (scTranscoding, bool) {
	for _, t := range list {
		if t.Format.Protocol == "progressive" && t.URL != "" {
			return t, true
		}
	}
	return scTranscoding{}, false
}

// scPlaylistResult turns a set object into a playlist result, applying the same
// maxTracks limit and truncated flag the Deezer source uses. SoundCloud fills in
// only the first few entries of a set and lists the rest as stubs carrying an id
// and nothing else, so each stub is read from /tracks/{id} before it is dropped.
func (c *SoundCloud) scPlaylistResult(ctx context.Context, set scTrack, maxTracks int) (protocol.LoadResult, error) {
	raw := set.Tracks
	truncated := maxTracks > 0 && len(raw) > maxTracks
	if truncated {
		raw = raw[:maxTracks]
	}
	full := c.hydrate(ctx, raw)
	tracks := make([]*protocol.Track, 0, len(full))
	for _, t := range full {
		v, err := scEncode(t)
		if err != nil {
			continue // an entry that cannot be played is skipped, as before
		}
		tracks = append(tracks, v)
	}
	if len(tracks) == 0 {
		return protocol.LoadResult{}, errors.New("SoundCloud returned no usable tracks")
	}
	name := strings.TrimSpace(set.Title)
	if name == "" {
		name = "SoundCloud playlist"
	}
	return protocol.LoadResult{LoadType: "playlist", Data: protocol.Playlist{
		Info:       protocol.PlaylistInfo{Name: name, SelectedTrack: -1},
		PluginInfo: protocol.Object{"type": "playlist", "truncated": truncated},
		Tracks:     tracks,
	}}, nil
}

// scStubConcurrency bounds how many stub entries are fetched at once. A set can
// list hundreds of entries, and SoundCloud is the same host the playback streams
// come from, so this stays small.
const scStubConcurrency = 4

// hydrate completes the entries a set response left as stubs. Entries that
// already carry a title are passed through untouched, and a stub that cannot be
// read is kept as it is so the caller decides whether it is usable.
func (c *SoundCloud) hydrate(ctx context.Context, raw []scTrack) []scTrack {
	out := make([]scTrack, len(raw))
	copy(out, raw)
	sem := make(chan struct{}, scStubConcurrency)
	var wg sync.WaitGroup
	for i, t := range out {
		if t.Title != "" || t.ID == 0 {
			continue
		}
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var full scTrack
			if err := c.fetch(ctx, c.api("/tracks/"+strconv.FormatInt(id, 10), nil), &full); err != nil {
				return
			}
			if full.ID != 0 {
				out[i] = full
			}
		}(i, t.ID)
	}
	wg.Wait()
	return out
}

// Load reads SoundCloud metadata for a search, a track link or a set link.
func (c *SoundCloud) Load(ctx context.Context, id string, searchResults, maxTracks int) (protocol.LoadResult, error) {
	if strings.HasPrefix(id, "scsearch:") {
		query := strings.TrimSpace(strings.TrimPrefix(id, "scsearch:"))
		if query == "" {
			return protocol.LoadResult{}, errors.New("search query is empty")
		}
		limit := searchResults
		if limit < 1 {
			limit = 1
		}
		if limit > 200 {
			limit = 200 // the endpoint refuses a larger limit
		}
		var page struct {
			TotalResults int       `json:"total_results"`
			Collection   []scTrack `json:"collection"`
		}
		if err := c.fetch(ctx, c.api("/search/tracks", url.Values{"q": {query}, "limit": {strconv.Itoa(limit)}, "offset": {"0"}}), &page); err != nil {
			return protocol.LoadResult{}, err
		}
		tracks := make([]*protocol.Track, 0, len(page.Collection))
		for _, t := range page.Collection {
			v, err := scEncode(t)
			if err != nil {
				continue // SoundCloud lists entries without a playable id
			}
			tracks = append(tracks, v)
		}
		if len(tracks) == 0 {
			return protocol.LoadResult{LoadType: "empty", Data: nil}, nil
		}
		return protocol.LoadResult{LoadType: "search", Data: tracks}, nil
	}
	link, err := scRef(id)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	var res scTrack
	if err = c.fetch(ctx, c.api("/resolve", url.Values{"url": {link}}), &res); err != nil {
		return protocol.LoadResult{}, err
	}
	switch {
	case res.Kind == "track" || (res.Kind == "" && res.ID != 0):
		track, err := scEncode(res)
		if err != nil {
			return protocol.LoadResult{}, err
		}
		return protocol.LoadResult{LoadType: "track", Data: track}, nil
	case strings.HasPrefix(res.Kind, "playlist"):
		return c.scPlaylistResult(ctx, res, maxTracks)
	default:
		return protocol.LoadResult{}, fmt.Errorf("SoundCloud returned a %q resource; only a track or a set can be loaded", res.Kind)
	}
}

// StreamURL resolves a track's progressive transcoding into a CDN URL. The URL
// is deliberately not cached: it is time limited, so every playback start
// resolves a new one, the same way the native YouTube path resolves per start.
func (c *SoundCloud) StreamURL(ctx context.Context, trackID string) (string, error) {
	if !scNumericID.MatchString(trackID) {
		return "", fmt.Errorf("expected a numeric SoundCloud track id, got %q", trackID)
	}
	var t scTrack
	if err := c.fetch(ctx, c.api("/tracks/"+trackID, nil), &t); err != nil {
		return "", err
	}
	// A track SoundCloud does not offer to this node, such as one its policy
	// blocks, comes back with no transcodings at all, which is a different
	// problem from a track that only offers hls.
	if len(t.Media.Transcodings) == 0 {
		return "", fmt.Errorf("SoundCloud track %s has no transcodings (policy %q), so it cannot be streamed from this node", trackID, t.Policy)
	}
	tr, ok := scProgressive(t.Media.Transcodings)
	if !ok {
		return "", fmt.Errorf("SoundCloud track %s has no progressive transcoding", trackID)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.fetch(ctx, tr.URL, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("SoundCloud did not resolve a stream URL for track %s", trackID)
	}
	return out.URL, nil
}

// loadSoundCloud prefers the native api-v2 paths and keeps yt-dlp as the
// fallback, the same way loadYouTube does. Both reasons are reported when
// neither path works.
func (s *Service) loadSoundCloud(ctx context.Context, id string) (protocol.LoadResult, error) {
	if !s.Config.SoundCloudNative {
		s.log().Debug("soundcloud resolution uses the yt-dlp path (native disabled)", "identifier", id)
		return s.loadYTDLP(ctx, id)
	}
	started := time.Now()
	native, nativeErr := s.SoundCloud.Load(ctx, id, s.Config.SearchResults, s.Config.MaxPlaylistTracks)
	if nativeErr == nil {
		s.log().Debug("soundcloud resolved natively", "identifier", id, "loadType", native.LoadType, "durationMs", time.Since(started).Milliseconds())
		return native, nil
	}
	s.log().Debug(s.ytdlpFallbackMessage("soundcloud"), "identifier", id, "durationMs", time.Since(started).Milliseconds(), "error", nativeErr)
	fallback, err := s.loadYTDLP(ctx, id)
	if err == nil {
		return fallback, nil
	}
	return protocol.LoadResult{}, fmt.Errorf("native soundcloud resolution failed (%v); yt-dlp fallback failed (%v)", nativeErr, err)
}

// openSoundCloud resolves the transcoding when playback starts, because a
// resolved CDN URL is valid only briefly and must never be baked into a track.
// openMedia keeps its usual retry: a rejected URL is replaced by a freshly
// resolved one rather than failing the playback. A track whose only transcoding
// is hls cannot be played natively, so yt-dlp is offered that case.
func (s *Service) openSoundCloud(ctx context.Context, t *protocol.Track) (io.ReadCloser, error) {
	work, cancel := context.WithTimeout(ctx, s.Config.LoadTimeout())
	defer cancel()
	select {
	case s.loads <- struct{}{}:
		defer func() { <-s.loads }()
	case <-work.Done():
		return nil, work.Err()
	}
	fallbackID := t.Info.Identifier
	if t.Info.URI != nil {
		fallbackID = *t.Info.URI
	}
	return s.openMedia(ctx, work, func(resolveCtx context.Context) (string, map[string]string, error) {
		started := time.Now()
		raw, err := s.SoundCloud.StreamURL(resolveCtx, t.Info.Identifier)
		if err == nil {
			s.log().Debug("soundcloud stream resolved natively", "trackId", t.Info.Identifier, "durationMs", time.Since(started).Milliseconds())
			return raw, nil, err
		}
		if !s.Config.YTDLPEnabled {
			return "", nil, fmt.Errorf("native soundcloud stream failed (%v); yt-dlp fallback is disabled (ytDlpEnabled: false)", err)
		}
		s.log().Debug(s.ytdlpFallbackMessage("soundcloud stream"),
			"trackId", t.Info.Identifier,
			"error", err,
		)
		fallback, headers, ferr := s.ytdlpMediaURL(resolveCtx, fallbackID)
		if ferr != nil {
			return "", nil, fmt.Errorf("native soundcloud stream failed (%v); yt-dlp fallback failed (%v)", err, ferr)
		}
		return fallback, headers, nil
	})
}
