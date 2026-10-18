package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golalink/internal/protocol"
)

// LRCLIBBase is the public lyrics API. It needs no account and answers with
// timed lyrics, which is what makes a line by line display possible rather than
// a wall of text.
const LRCLIBBase = "https://lrclib.net/api"

const (
	// lrclibAgent identifies this node. The service asks callers to say who they
	// are so it can tell them apart.
	lrclibAgent = "Golalink (https://github.com/Golalink/Golalink)"
	// lrclibTimeout bounds one lookup. A client waiting for lyrics would rather
	// have no answer than a slow one.
	lrclibTimeout = 10 * time.Second
	// lrclibBody bounds a response, so a hostile or broken answer cannot fill
	// memory.
	lrclibBody = 1 << 20
	// lrclibRetryDelay is how long to wait before the one retry.
	lrclibRetryDelay = 250 * time.Millisecond
	// lrclibSlack is how far a search result's length may be from the track's
	// before it is treated as a different recording.
	lrclibSlack = 4000
)

// LRCLIB fetches lyrics from lrclib.net.
type LRCLIB struct {
	// Client is the HTTP client to use. Nil means the default one.
	Client *http.Client
	// Base is the API root, so a test can point it at its own server.
	Base string
}

// NewLRCLIB returns a provider using the given client.
func NewLRCLIB(client *http.Client) *LRCLIB {
	return &LRCLIB{Client: client, Base: LRCLIBBase}
}

func (l *LRCLIB) Name() string { return "lrclib" }

// lrclibTrack is the part of a response this uses.
type lrclibTrack struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	Synced       string  `json:"syncedLyrics"`
	Plain        string  `json:"plainLyrics"`
}

func (l *LRCLIB) Fetch(ctx context.Context, t *protocol.Track) (*Lyrics, error) {
	artist, title := lrclibNames(t)
	if title == "" {
		return nil, ErrNotFound
	}
	var last error
	// The exact lookup needs the same artist, title and length the database was
	// given, which a video title rarely matches. It is tried first because it is
	// the one that cannot pick the wrong recording.
	if v, err := l.get(ctx, artist, title, t.Info.Length); err == nil {
		return l.build(t, v)
	} else if !errors.Is(err, ErrNotFound) {
		last = err
	}
	// The name a video credits is often not the name the database filed the
	// track under, and an artist that matches nothing drags a combined query down
	// with it. So the title alone is tried too, and the length decides between
	// the recordings that share it.
	//
	// A query that fails is not the end of the search. The service answers 503
	// for some queries and not others, and giving up on the first one threw away
	// the answer the next query would have found.
	for _, q := range lrclibQueries(t, artist, title) {
		found, err := l.search(ctx, q)
		if err != nil && !errors.Is(err, ErrNotFound) {
			last = err
			continue
		}
		if best := lrclibBest(found, t.Info.Length); best != nil {
			return l.build(t, *best)
		}
	}
	if last != nil {
		return nil, last
	}
	return nil, ErrNotFound
}

// lrclibQueries lists what to search for, in the order worth trying. The first
// two are the obvious ones; the last is for the uploads that put the song's own
// name in brackets after the artist, where the name on its own is what the
// database knows it by.
func lrclibQueries(t *protocol.Track, artist, title string) []string {
	queries := []string{strings.TrimSpace(artist + " " + title), title}
	if extra := lrclibBracketed(t.Info.Title); extra != "" && extra != title {
		queries = append(queries, extra)
	}
	return queries
}

// lrclibBracketed returns the bracketed parts of a title that are not a credit,
// joined. A bracket holding only a credit is skipped, because searching for it
// would find nothing.
func lrclibBracketed(title string) string {
	var parts []string
	for _, pair := range [][2]string{{"【", "】"}, {"（", "）"}, {"(", ")"}, {"[", "]"}} {
		rest := title
		for {
			open := strings.Index(rest, pair[0])
			if open < 0 {
				break
			}
			after := rest[open+len(pair[0]):]
			closing := strings.Index(after, pair[1])
			if closing < 0 {
				break
			}
			if inner := strings.TrimSpace(after[:closing]); inner != "" && !containsNoise(inner) {
				parts = append(parts, inner)
			}
			rest = after[closing+len(pair[1]):]
		}
	}
	return strings.Join(parts, " ")
}

// build turns a database entry into the payload a client reads. Timed lyrics are
// preferred, because a client can follow them; plain ones are still worth
// returning, because a wall of text is better than nothing.
func (l *LRCLIB) build(t *protocol.Track, v lrclibTrack) (*Lyrics, error) {
	out := &Lyrics{SourceName: t.Info.SourceName, Provider: l.Name(), Plugin: protocol.Object{}}
	if synced := strings.TrimSpace(v.Synced); synced != "" {
		if lines := parseLRC(synced); len(lines) > 0 {
			out.Lines = lines
			return out, nil
		}
	}
	if plain := strings.TrimSpace(v.Plain); plain != "" {
		out.Text = &plain
		return out, nil
	}
	return nil, ErrNotFound
}

func (l *LRCLIB) get(ctx context.Context, artist, title string, length int64) (lrclibTrack, error) {
	q := url.Values{}
	q.Set("artist_name", artist)
	q.Set("track_name", title)
	if length > 0 {
		q.Set("duration", fmt.Sprintf("%d", (length+500)/1000))
	}
	var v lrclibTrack
	if err := l.do(ctx, "/get?"+q.Encode(), &v); err != nil {
		return lrclibTrack{}, err
	}
	return v, nil
}

func (l *LRCLIB) search(ctx context.Context, q string) ([]lrclibTrack, error) {
	var v []lrclibTrack
	if err := l.do(ctx, "/search?q="+url.QueryEscape(q), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// do performs one lookup, retrying once. The service is free and shared, so a
// single dropped request should not become a track the client shows no lyrics
// for. A 404 is the service answering that it has nothing, which is not a
// failure and is not retried.
func (l *LRCLIB) do(ctx context.Context, path string, dest any) error {
	ctx, cancel := context.WithTimeout(ctx, lrclibTimeout)
	defer cancel()
	err := l.once(ctx, path, dest)
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(lrclibRetryDelay):
	}
	return l.once(ctx, path, dest)
}

// once performs a single request and decodes the answer.
func (l *LRCLIB) once(ctx context.Context, path string, dest any) error {
	base := l.Base
	if base == "" {
		base = LRCLIBBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", lrclibAgent)
	req.Header.Set("Accept", "application/json")
	client := l.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lyrics: lrclib answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, lrclibBody))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("lyrics: lrclib sent an unreadable answer: %w", err)
	}
	return nil
}

// lrclibBest picks the entry that is most likely to be the same recording. A
// search for a song returns covers and live takes as well, and the length is what
// tells them apart when the names are the same.
func lrclibBest(found []lrclibTrack, length int64) *lrclibTrack {
	var best *lrclibTrack
	bestScore := -1
	for i := range found {
		v := &found[i]
		if strings.TrimSpace(v.Synced) == "" && strings.TrimSpace(v.Plain) == "" {
			continue
		}
		score := 0
		if v.Instrumental {
			score--
		}
		if strings.TrimSpace(v.Synced) != "" {
			score += 2
		}
		if length > 0 && v.Duration > 0 {
			off := int64(v.Duration*1000) - length
			if off < 0 {
				off = -off
			}
			switch {
			case off <= 2000:
				score += 4
			case off <= lrclibSlack:
				score++
			default:
				score -= 4
			}
		}
		if score > bestScore {
			best, bestScore = v, score
		}
	}
	return best
}

// lrclibNoise are the words that describe a video rather than a song, so a
// bracketed part of a title that contains one of them is not part of the name the
// database was given.
var lrclibNoise = []string{
	"official", "video", "audio", "lyric", "mv", "m/v", "hd", "4k",
	"visualizer", "visualiser", "remaster", "explicit", "color coded",
}

// lrclibNames splits a track into the artist and title a lyrics database would
// have been given. A video title carries a lot that is not part of the song, so
// the parts that only describe the video, the decoration and the list of who
// else is on the track are all dropped first.
func lrclibNames(t *protocol.Track) (artist, title string) {
	artist = strings.TrimSpace(t.Info.Author)
	title = stripSymbols(stripTitleNoise(strings.TrimSpace(t.Info.Title)))
	title = stripFeatured(title)
	title = stripTrailer(title)
	// A source that reports no artist often puts one in the title, and the
	// database keeps the two apart.
	if artist == "" {
		if at := strings.Index(title, " - "); at > 0 {
			artist, title = strings.TrimSpace(title[:at]), strings.TrimSpace(title[at+3:])
		}
	}
	return artist, title
}

// lrclibFeatured are the words after which a title is naming who else is on the
// track rather than what it is called. "with" is deliberately not one of them,
// because it is just as likely to be part of the name.
var lrclibFeatured = []string{" ft.", " ft ", " feat.", " feat ", " featuring "}

// stripFeatured cuts a title at the featured artist, which is not part of the
// name the database was given.
func stripFeatured(title string) string {
	lower := strings.ToLower(title)
	cut := len(title)
	for _, marker := range lrclibFeatured {
		if at := strings.Index(lower, marker); at > 0 && at < cut {
			cut = at
		}
	}
	return strings.Join(strings.Fields(title[:cut]), " ")
}

// lrclibTrailer are the words that describe the video rather than the song when
// they sit at the end of a title, which is where a credit would normally be
// bracketed and is not always.
var lrclibTrailer = map[string]bool{
	"official": true, "music": true, "video": true, "audio": true,
	"lyric": true, "lyrics": true, "mv": true, "m/v": true, "pv": true,
	"hd": true, "4k": true, "8k": true, "full": true, "ver": true,
	"version": true, "visualizer": true, "visualiser": true,
	"remaster": true, "remastered": true, "explicit": true,
}

// stripTrailer takes the description off the end of a title, such as the
// "Official Music Video" that follows the name without brackets. Words come off
// only while every one of them is description, so a title that is nothing else
// is left as it is.
func stripTrailer(title string) string {
	words := strings.Fields(title)
	keep := len(words)
	for keep > 0 && lrclibTrailer[strings.ToLower(words[keep-1])] {
		keep--
	}
	if keep == 0 {
		return title
	}
	return strings.Join(words[:keep], " ")
}

// stripSymbols removes the emoji and other decoration a video title carries,
// which a lyrics database has never heard of. Letters in any script are kept, so
// a title written in Japanese survives intact.
func stripSymbols(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsSpace(r):
			b.WriteRune(r)
		case strings.ContainsRune("-'&!?.,:;/()[]", r):
			b.WriteRune(r)
		default:
			// A dropped character becomes a space rather than nothing, so
			// decoration sitting between two words does not run them together.
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// stripTitleNoise removes the bracketed parts of a title that describe the video.
// A bracket that is part of the song, such as a remix or a reprise, is left
// alone, because those words change what the lyrics are.
func stripTitleNoise(title string) string {
	var b strings.Builder
	for i := 0; i < len(title); {
		open := title[i]
		if open != '(' && open != '[' {
			b.WriteByte(open)
			i++
			continue
		}
		closing := byte(')')
		if open == '[' {
			closing = ']'
		}
		end := strings.IndexByte(title[i:], closing)
		if end < 0 {
			b.WriteString(title[i:])
			break
		}
		if !containsNoise(title[i+1 : i+end]) {
			b.WriteString(title[i : i+end+1])
		}
		i += end + 1
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func containsNoise(s string) bool {
	lower := strings.ToLower(s)
	for _, w := range lrclibNoise {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}
