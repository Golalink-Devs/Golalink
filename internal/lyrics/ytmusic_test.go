package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"golalink/internal/protocol"
)

// ytCatalogue answers the two endpoints the way the live catalogue does, with a
// different answer per client identity. An empty answer is what the service
// sends when it has nothing, which it does by answering 404.
type ytCatalogue struct {
	next   string
	mobile string
	web    string
	// status, when set, is answered to every request instead.
	status int
	calls  calls
}

func (c *ytCatalogue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.calls.record(r)
	if c.status != 0 {
		w.WriteHeader(c.status)
		return
	}
	var body string
	switch {
	case strings.HasSuffix(r.URL.Path, "/next"):
		body = c.next
	case strings.HasSuffix(r.URL.Path, "/browse"):
		switch r.Header.Get("X-YouTube-Client-Name") {
		case "21":
			body = c.mobile
		case "67":
			body = c.web
		}
	}
	if body == "" {
		http.NotFound(w, r)
		return
	}
	io.WriteString(w, body)
}

// ytAt points a provider at a test catalogue. Endpoint and Client exist so the
// two requests can be exercised without the public service.
func ytAt(t *testing.T, c *ytCatalogue) *YTMusic {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return &YTMusic{Endpoint: srv.URL, Client: srv.Client()}
}

const ytNextWithLyrics = `{
  "contents": {
    "singleColumnMusicWatchNextResultsRenderer": {
      "tabbedRenderer": {
        "watchNextTabbedResultsRenderer": {
          "tabs": [
            {
              "tabRenderer": {
                "endpoint": {
                  "browseEndpoint": {
                    "browseId": "MPLYtrack",
                    "browseEndpointContextSupportedConfigs": {
                      "browseEndpointContextMusicConfig": {
                        "pageType": "MUSIC_PAGE_TYPE_TRACK_LYRICS"
                      }
                    }
                  }
                }
              }
            },
            {
              "tabRenderer": {
                "endpoint": {
                  "browseEndpoint": {
                    "browseId": "RELATED",
                    "browseEndpointContextSupportedConfigs": {
                      "browseEndpointContextMusicConfig": {
                        "pageType": "MUSIC_PAGE_TYPE_TRACK_RELATED"
                      }
                    }
                  }
                }
              }
            }
          ]
        }
      }
    }
  }
}`

const ytNextWithoutLyrics = `{
  "contents": {
    "singleColumnMusicWatchNextResultsRenderer": {
      "tabbedRenderer": {
        "watchNextTabbedResultsRenderer": {
          "tabs": [
            {
              "tabRenderer": {
                "endpoint": {
                  "browseEndpoint": {
                    "browseId": "RELATED",
                    "browseEndpointContextSupportedConfigs": {
                      "browseEndpointContextMusicConfig": {
                        "pageType": "MUSIC_PAGE_TYPE_TRACK_RELATED"
                      }
                    }
                  }
                }
              }
            }
          ]
        }
      }
    }
  }
}`

const ytMobileTimed = `{
  "contents": {
    "elementRenderer": {
      "newElement": {
        "type": {
          "componentType": {
            "model": {
              "timedLyricsModel": {
                "lyricsData": {
                  "sourceMessage": "Source: LyricFind",
                  "timedLyricsData": [
                    {
                      "lyricLine": "one",
                      "cueRange": {
                        "startTimeMilliseconds": "1000",
                        "endTimeMilliseconds": "2500"
                      }
                    },
                    {
                      "lyricLine": "two",
                      "cueRange": {
                        "startTimeMilliseconds": "3000",
                        "endTimeMilliseconds": "4000"
                      }
                    }
                  ]
                }
              }
            }
          }
        }
      }
    }
  }
}`

// ytMobileUntimed is the shape the catalogue sends when it has the words but not
// when they are sung: a line carries no cue.
const ytMobileUntimed = `{
  "contents": {
    "elementRenderer": {
      "newElement": {
        "type": {
          "componentType": {
            "model": {
              "timedLyricsModel": {
                "lyricsData": {
                  "timedLyricsData": [
                    {
                      "lyricLine": "one",
                      "cueRange": {
                        "startTimeMilliseconds": "1000",
                        "endTimeMilliseconds": "2500"
                      }
                    },
                    {
                      "lyricLine": "two"
                    }
                  ]
                }
              }
            }
          }
        }
      }
    }
  }
}`

const ytWebText = `{
  "contents": {
    "sectionListRenderer": {
      "contents": [
        {
          "musicDescriptionShelfRenderer": {
            "description": {
              "runs": [{"text": "whole text"}]
            },
            "footer": {
              "runs": [{"text": "Source: "}, {"text": "LyricFind"}]
            }
          }
        }
      ]
    }
  }
}`

func TestYTMusicTimedAnswer(t *testing.T) {
	cat := &ytCatalogue{next: ytNextWithLyrics, mobile: ytMobileTimed, web: ytWebText}
	got, err := ytAt(t, cat).Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Provider != "ytmusic" || got.SourceName != "youtube" {
		t.Fatalf("got provider %q source %q", got.Provider, got.SourceName)
	}
	if got.Text != nil {
		t.Fatalf("a timed answer also carried a text body %q", *got.Text)
	}
	want := []Line{timed(1000, "one", 1500), timed(3000, "two", 1000)}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("got %s, want %s", showLines(got.Lines), showLines(want))
	}
	if source := got.Plugin["source"]; source != "LyricFind" {
		t.Fatalf("plugin source is %v, want the licensor the catalogue credited", source)
	}

	// The timed answer only comes back to the Android identity, so the browse
	// that follows the page lookup must be the mobile one, at the version the
	// timings were verified against. A version the catalogue no longer answers
	// with timings for would silently turn every track into plain text.
	reqs := cat.calls.all()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want the page lookup and the mobile browse", len(reqs))
	}
	if v := reqs[1].header.Get("X-YouTube-Client-Version"); v != "7.21.50" {
		t.Fatalf("asked with Android version %q, want 7.21.50", v)
	}
	if name := reqs[1].header.Get("X-YouTube-Client-Name"); name != "21" {
		t.Fatalf("asked as client %q, want the Android app", name)
	}
	// The page the words live on is named by the device-neutral request, which
	// stays the web identity.
	if name := reqs[0].header.Get("X-YouTube-Client-Name"); name != "67" {
		t.Fatalf("the page lookup asked as client %q, want the web app", name)
	}
}

func TestYTMusicUntimedAnswerBecomesText(t *testing.T) {
	// One line without a cue means the words are there but the timings are not,
	// which is still worth returning; asking the web app instead would only
	// fetch the same words as one run.
	cat := &ytCatalogue{next: ytNextWithLyrics, mobile: ytMobileUntimed}
	got, err := ytAt(t, cat).Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Lines) != 0 {
		t.Fatalf("got %d timed lines for an answer without timings", len(got.Lines))
	}
	if got.Text == nil || *got.Text != "one\ntwo" {
		t.Fatalf("got text %v, want the lines joined", got.Text)
	}
	if n := cat.calls.count("/browse"); n != 1 {
		t.Fatalf("made %d browse requests, want the mobile one only", n)
	}
}

func TestYTMusicWebShelfIsTheFallback(t *testing.T) {
	// The web app is given the words as one run inside a description shelf, and
	// that shape is only asked for when the mobile answer had nothing.
	cat := &ytCatalogue{next: ytNextWithLyrics, mobile: `{"contents": {}}`, web: ytWebText}
	got, err := ytAt(t, cat).Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Text == nil || *got.Text != "whole text" {
		t.Fatalf("got text %v, want the shelf", got.Text)
	}
	if len(got.Lines) != 0 {
		t.Fatalf("got %d timed lines from a text answer", len(got.Lines))
	}
	if source := got.Plugin["source"]; source != "LyricFind" {
		t.Fatalf("plugin source is %v, want the footer's credit", source)
	}
	reqs := cat.calls.all()
	if len(reqs) != 3 {
		t.Fatalf("made %d requests, want the page lookup and both browse shapes", len(reqs))
	}
	if name := reqs[2].header.Get("X-YouTube-Client-Name"); name != "67" {
		t.Fatalf("the fallback asked as client %q, want the web app", name)
	}
}

func TestYTMusicNoLyrics(t *testing.T) {
	t.Run("a track with no lyrics tab", func(t *testing.T) {
		cat := &ytCatalogue{next: ytNextWithoutLyrics, mobile: ytMobileTimed}
		if _, err := ytAt(t, cat).Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		// No page was named, so there is nothing to browse for.
		if n := cat.calls.count("/browse"); n != 0 {
			t.Fatalf("browsed %d times without a lyrics page", n)
		}
	})
	t.Run("a missing page", func(t *testing.T) {
		cat := &ytCatalogue{mobile: ytMobileTimed}
		if _, err := ytAt(t, cat).Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		if n := cat.calls.count("/browse"); n != 0 {
			t.Fatalf("browsed %d times when the lookup found no page", n)
		}
	})
}

// TestYTMusicOnlyAsksAboutYouTubeTracks keeps a track the catalogue has no
// handle for from spending two requests to learn nothing.
func TestYTMusicOnlyAsksAboutYouTubeTracks(t *testing.T) {
	cases := []struct {
		name string
		tr   *protocol.Track
	}{
		{"another source", track("soundcloud", "dQw4w9WgXcQ", "", "", 0)},
		{"an id that is too short", track("youtube", "dQw4w9WgXc", "", "", 0)},
		{"an id that is too long", track("youtube", "dQw4w9WgXcQ1", "", "", 0)},
		{"an id with a character a video id cannot hold", track("youtube", "dQw4w9WgX!Q", "", "", 0)},
		{"no id at all", track("youtube", "", "", "", 0)},
		{"no track at all", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cat := &ytCatalogue{next: ytNextWithLyrics, mobile: ytMobileTimed}
			if _, err := ytAt(t, cat).Fetch(context.Background(), c.tr); !errors.Is(err, ErrNotFound) {
				t.Fatalf("got %v, want ErrNotFound", err)
			}
			if reqs := cat.calls.all(); len(reqs) != 0 {
				t.Fatalf("made %d requests for a track with no catalogue handle", len(reqs))
			}
		})
	}
}

// TestYTMusicServiceFailureIsNotAnAnswer keeps a broken catalogue from being
// reported to the client as a track that has no lyrics.
func TestYTMusicServiceFailureIsNotAnAnswer(t *testing.T) {
	cat := &ytCatalogue{status: http.StatusServiceUnavailable}
	_, err := ytAt(t, cat).Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0))
	if err == nil {
		t.Fatal("a failing catalogue answered with no error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a failure rather than no lyrics", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("got %v, want the status the catalogue answered with", err)
	}
}

// TestYTMusicVersionOverride pins that the override reaches the web identity
// only. The Android version is what selects the timed answer, so overriding it
// would silently lose the timings the mobile request exists to fetch.
func TestYTMusicVersionOverride(t *testing.T) {
	cat := &ytCatalogue{next: ytNextWithLyrics, mobile: `{"contents": {}}`, web: ytWebText}
	p := ytAt(t, cat)
	p.Version = "9.9.9"
	if _, err := p.Fetch(context.Background(), track("youtube", "dQw4w9WgXcQ", "", "", 0)); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var versions []string
	for _, r := range cat.calls.all() {
		versions = append(versions, r.header.Get("X-YouTube-Client-Version"))
	}
	want := []string{"9.9.9", "7.21.50", "9.9.9"}
	if !reflect.DeepEqual(versions, want) {
		t.Fatalf("sent versions %q, want %q", versions, want)
	}
	// The identity travels in the body as well as the headers.
	if body := cat.calls.all()[0].body; !strings.Contains(body, `"clientVersion":"9.9.9"`) {
		t.Fatalf("the page lookup body did not carry the configured version: %s", body)
	}
}

func TestYTMillisReadsStringsAndNumbers(t *testing.T) {
	// The catalogue sends a millisecond count as a string in the timed answer and
	// as a number elsewhere. A count that cannot be read is zero rather than an
	// error, because one unreadable timestamp must not cost the track its lyrics.
	cases := []struct {
		json string
		want int64
	}{
		{`{"t":"1500"}`, 1500},
		{`{"t":1500}`, 1500},
		{`{"t":"0"}`, 0},
		{`{"t":""}`, 0},
		{`{"t":null}`, 0},
		{`{"t":"garbage"}`, 0},
		{`{}`, 0},
	}
	for _, c := range cases {
		t.Run(c.json, func(t *testing.T) {
			var doc struct {
				T ytMillis `json:"t"`
			}
			if err := json.Unmarshal([]byte(c.json), &doc); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if int64(doc.T) != c.want {
				t.Fatalf("read %d, want %d", doc.T, c.want)
			}
		})
	}
}
