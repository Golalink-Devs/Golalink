package lyrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// lrclibAt points a provider at a test server. Base and Client exist so the
// lookup can be exercised without the public service.
func lrclibAt(t *testing.T, h http.HandlerFunc) *LRCLIB {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &LRCLIB{Base: srv.URL, Client: srv.Client()}
}

func TestLRCLIBSyncedAnswer(t *testing.T) {
	const body = `{"trackName":"Song","artistName":"Artist","duration":210,` +
		`"syncedLyrics":"[00:01.00]first line\n[00:04.50]second line\n"}`
	seen := &calls{}
	p := lrclibAt(t, func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		io.WriteString(w, body)
	})

	// The length is given in milliseconds and the service is asked in whole
	// seconds, rounded, so 210.4s must not become 210.9 or 210s exactly.
	got, err := p.Fetch(context.Background(), track("youtube", "vid", "Artist", "Song", 210400))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Provider != "lrclib" || got.SourceName != "youtube" {
		t.Fatalf("got provider %q source %q", got.Provider, got.SourceName)
	}
	if got.Text != nil {
		t.Fatalf("timed lyrics also carried a text body %q", *got.Text)
	}
	want := []Line{timed(1000, "first line", 3500), timed(4500, "second line")}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("got %s, want %s", showLines(got.Lines), showLines(want))
	}

	reqs := seen.all()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want the exact lookup only", len(reqs))
	}
	q := reqs[0].query
	if q.Get("artist_name") != "Artist" || q.Get("track_name") != "Song" {
		t.Fatalf("asked for artist %q track %q", q.Get("artist_name"), q.Get("track_name"))
	}
	if q.Get("duration") != "210" {
		t.Fatalf("asked with duration %q, want the rounded whole seconds", q.Get("duration"))
	}
}

func TestLRCLIBPlainAnswer(t *testing.T) {
	// Timed lyrics are preferred and text is the fallback, but a synced field
	// that holds no readable line is as good as absent, so the text must still
	// be returned rather than the track being reported as having no lyrics.
	cases := []struct {
		name string
		body string
	}{
		{"no synced lyrics", `{"plainLyrics":"first line\nsecond line","syncedLyrics":""}`},
		{"synced lyrics with no stamps", `{"plainLyrics":"first line\nsecond line","syncedLyrics":"first line\nsecond line"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := lrclibAt(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, c.body)
			})
			got, err := p.Fetch(context.Background(), track("youtube", "vid", "Artist", "Song", 0))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if len(got.Lines) != 0 {
				t.Fatalf("got %d lines for an answer with no times", len(got.Lines))
			}
			if got.Text == nil || *got.Text != "first line\nsecond line" {
				t.Fatalf("got text %v, want the plain words", got.Text)
			}
		})
	}
}

// TestLRCLIBSearchChoosesTheRecording covers the fallback after the exact lookup
// misses: the queries are tried in order and the length decides between entries
// that share a name, so a cover or a live take is not returned for the track.
func TestLRCLIBSearchChoosesTheRecording(t *testing.T) {
	seen := &calls{}
	p := lrclibAt(t, func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		if strings.HasSuffix(r.URL.Path, "/get") {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("q") {
		case "Artist Song":
			// A cover with the same name and a length no recording shares: it
			// is not the track, so the search moves on to the next query.
			io.WriteString(w, `[{"trackName":"Song","artistName":"Cover Band","duration":180,"plainLyrics":"wrong words"}]`)
		case "Song":
			// Two entries of the right length: the one without the instrumental
			// flag is the one with words to show.
			io.WriteString(w, `[`+
				`{"trackName":"Song","artistName":"Artist","duration":210.4,"instrumental":true,"syncedLyrics":"[00:02.00]instrumental words"},`+
				`{"trackName":"Song","artistName":"Artist","duration":210.4,"syncedLyrics":"[00:03.00]right words"}`+
				`]`)
		default:
			http.NotFound(w, r)
		}
	})

	got, err := p.Fetch(context.Background(), track("youtube", "vid", "Artist", "Song", 210400))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := []Line{timed(3000, "right words")}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("got %s, want %s", showLines(got.Lines), showLines(want))
	}

	var queries []string
	for _, r := range seen.all() {
		if strings.HasSuffix(r.path, "/search") {
			queries = append(queries, r.query.Get("q"))
		}
	}
	if want := []string{"Artist Song", "Song"}; !reflect.DeepEqual(queries, want) {
		t.Fatalf("searched %q, want %q", queries, want)
	}
}

func TestLRCLIBQueries(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		artist string
		title  string
		want   []string
	}{
		{
			// A video title that names its own description in brackets gives the
			// database nothing new to search for.
			name: "a description bracket adds no query", raw: "Song (Official Video)",
			artist: "Artist", title: "Song",
			want: []string{"Artist Song", "Song"},
		},
		{
			// An upload that puts the song's real name in brackets is searched
			// for by that name last, after the obvious two have failed.
			name: "a bracket that names the song adds a query", raw: "Artist - Song (Acoustic Version)",
			artist: "Artist", title: "Song",
			want: []string{"Artist Song", "Song", "Acoustic Version"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := track("youtube", "vid", c.artist, c.raw, 0)
			got := lrclibQueries(tr, c.artist, c.title)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestLRCLIBBracketedParts(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Song (Official Video)", ""},
		{"Song [Audio]", ""},
		{"Song (color coded)", ""},
		{"Song (Remastered 2011)", ""},
		// A live take is a different recording, so its bracket is part of the
		// name worth searching for.
		{"Song (Live) [4K]", "Live"},
		{"Song (Acoustic) (Remix)", "Acoustic Remix"},
		{"Song 【Remix】", "Remix"},
		{"Song （アコースティック）", "アコースティック"},
		{"Song (unclosed", ""},
		{"No brackets at all", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := lrclibBracketed(c.in); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestLRCLIBTrackNames(t *testing.T) {
	cases := []struct {
		name           string
		author, title  string
		wantArtist     string
		wantTrackTitle string
	}{
		{"a described title is cut to the song", "Artist", "Song (Official Music Video)", "Artist", "Song"},
		{"a square bracket description goes too", "Artist", "Song [Official Video]", "Artist", "Song"},
		{"a featured artist is a credit, not the name", "Artist", "Song feat. Guest", "Artist", "Song"},
		// A live take is what the words belong to, so the bracket is kept.
		{"a live bracket stays", "Artist", "Song (Live)", "Artist", "Song (Live)"},
		{"an artist in the title is split off when the source has none", "", "Artist - Song", "Artist", "Song"},
		// Letters in any script survive; only decoration is dropped, and it
		// becomes a space so the words around it stay apart.
		{"a Japanese title survives", "Artist", "夜に駆ける", "Artist", "夜に駆ける"},
		{"runs of spaces are collapsed", "Artist", "  Song   Name  ", "Artist", "Song Name"},
		{"a punctuation mark inside the name is kept", "Artist", "Don't Stop Me Now", "Artist", "Don't Stop Me Now"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			artist, title := lrclibNames(track("youtube", "vid", c.author, c.title, 0))
			if artist != c.wantArtist || title != c.wantTrackTitle {
				t.Fatalf("got artist %q title %q, want %q and %q", artist, title, c.wantArtist, c.wantTrackTitle)
			}
		})
	}
}

func TestLRCLIBBestEntry(t *testing.T) {
	const near, far = 210.4, 260.0
	cases := []struct {
		name   string
		found  []lrclibTrack
		length int64
		want   int // -1 means no entry is worth returning
	}{
		{
			name:   "timed words are worth more than plain ones of the same length",
			found:  []lrclibTrack{{Synced: "[00:01.00]a", Duration: near}, {Plain: "a", Duration: near}},
			length: 210400,
			want:   0,
		},
		{
			// A recording of the wrong length is a different take whatever it
			// offers, so plain words that match the length win.
			name:   "the length decides before the form the words take",
			found:  []lrclibTrack{{Synced: "[00:01.00]a", Duration: 213.4}, {Plain: "b", Duration: near}},
			length: 210400,
			want:   1,
		},
		{
			name:   "an instrumental loses to the same entry with words",
			found:  []lrclibTrack{{Synced: "[00:01.00]a", Duration: near, Instrumental: true}, {Synced: "[00:01.00]b", Duration: near}},
			length: 210400,
			want:   1,
		},
		{
			// Entries with nothing to show are skipped rather than chosen as the
			// best of a bad set.
			name:   "entries without words are skipped",
			found:  []lrclibTrack{{Duration: near}, {Synced: "[00:01.00]b", Duration: near}},
			length: 210400,
			want:   1,
		},
		{
			name:   "an answer with no words at all has no best entry",
			found:  []lrclibTrack{{Duration: near, Instrumental: true}},
			length: 210400,
			want:   -1,
		},
		{
			// Without a length to compare against, the scoring must not throw
			// away the only answer.
			name:   "no length means the form is the only tie breaker",
			found:  []lrclibTrack{{Plain: "a", Duration: near}, {Synced: "[00:01.00]b", Duration: far}},
			length: 0,
			want:   1,
		},
		{
			// A length far from the track's is a reason to reject the only
			// entry rather than return words for a different recording.
			name:   "a length nothing like the track's rejects the entry",
			found:  []lrclibTrack{{Synced: "[00:01.00]a", Duration: far}},
			length: 210400,
			want:   -1,
		},
		{name: "an empty answer has no best entry", found: nil, length: 210400, want: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lrclibBest(c.found, c.length)
			if c.want < 0 {
				if got != nil {
					t.Fatal("chose an entry, want none")
				}
				return
			}
			if got == nil {
				t.Fatalf("chose no entry, want entry %d", c.want)
			}
			if i := indexOf(c.found, got); i != c.want {
				t.Fatalf("chose entry %d, want %d", i, c.want)
			}
		})
	}
}

// indexOf locates the chosen entry by identity, so a case can name the one it
// expected even when two entries read alike.
func indexOf(found []lrclibTrack, v *lrclibTrack) int {
	for i := range found {
		if &found[i] == v {
			return i
		}
	}
	return -1
}

// TestLRCLIBNothingFound pins that a service saying it has nothing is an answer,
// not a failure, and that it is not retried: a 404 is the service answering.
func TestLRCLIBNothingFound(t *testing.T) {
	seen := &calls{}
	p := lrclibAt(t, func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		http.NotFound(w, r)
	})
	_, err := p.Fetch(context.Background(), track("youtube", "vid", "Artist", "Song", 0))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if n := seen.count("/get"); n != 1 {
		t.Fatalf("the exact lookup was made %d times, want once", n)
	}
	// The two queries are the artist and title, then the title alone.
	if n := seen.count("/search"); n != 2 {
		t.Fatalf("the search was made %d times, want the two queries once each", n)
	}
}

// TestLRCLIBNoTitleSkipsTheLookup keeps a track with no title from spending a
// request on a query that cannot match anything.
func TestLRCLIBNoTitleSkipsTheLookup(t *testing.T) {
	seen := &calls{}
	p := lrclibAt(t, func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		http.NotFound(w, r)
	})
	if _, err := p.Fetch(context.Background(), track("youtube", "vid", "Artist", "", 0)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if reqs := seen.all(); len(reqs) != 0 {
		t.Fatalf("made %d requests for a track with no title", len(reqs))
	}
}
