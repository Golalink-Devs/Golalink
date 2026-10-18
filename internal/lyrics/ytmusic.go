package lyrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golalink/internal/protocol"
)

// YouTube Music carries its own lyrics, and reaching them takes two requests
// rather than one:
//
//   - A watch-next request names the lyrics page and nothing else. The tab is
//     there but empty, holding only a browseId.
//   - The lyrics page is then asked for, and it answers differently depending
//     on who asks. The mobile app is given the timed lines a player follows,
//     the web app the whole text as a single run.
//
// The mobile answer is asked for first because a client can follow timed lines
// and cannot follow a wall of text. Both shapes were verified against the live
// API on 2026-10-06, which is also where the client versions below come from.
const (
	// YTMusicEndpoint is the catalogue host both clients post to.
	YTMusicEndpoint = "https://music.youtube.com"

	// ytMusicVersion is the web client version, the same one the source package
	// searches the catalogue with. Such versions age quickly, so
	// Config.YoutubeClientVersion overrides it.
	ytMusicVersion = "1.20250101.01.00"
	// ytMusicMobileVersion is the Android app version that is still answered
	// with timed lines. The current app build is not required for this: the
	// catalogue keeps answering the version the library clients send.
	ytMusicMobileVersion = "7.21.50"

	ytMusicWebAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	ytMusicMobileAgent = "com.google.android.apps.youtube.music/7.21.50 (Linux; U; Android 11) gzip"

	// ytMusicTimeout bounds one request. The mobile answer is a few hundred
	// kilobytes, and a client waiting for lyrics would rather have no answer
	// than a slow one.
	ytMusicTimeout = 10 * time.Second
	// ytMusicBody bounds a response, so a hostile or broken answer cannot fill
	// memory.
	ytMusicBody = 8 << 20

	// ytMusicLyricsPage marks the tab that holds the words. The title of the
	// tab is localised, so the page type is what identifies it.
	ytMusicLyricsPage = "MUSIC_PAGE_TYPE_TRACK_LYRICS"
)

// ytVideoIDPattern matches a YouTube video id, which is the only handle the
// catalogue has for a track.
var ytVideoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ytClient is one of the two identities the lyrics page is asked with. They are
// the same endpoint and the same page; only the answer differs.
type ytClient struct {
	name    string
	version string
	agent   string
	// id is the numeric client id YouTube expects in X-YouTube-Client-Name.
	id string
	// mobile adds the Android fields the app sends, which is what selects the
	// answer that carries the timings.
	mobile bool
}

var (
	ytMusicWeb = ytClient{name: "WEB_REMIX", version: ytMusicVersion, agent: ytMusicWebAgent, id: "67"}
	// ytMusicMobile is read as the Android app. Its version is pinned rather
	// than configured: the timed answer is tied to the identity, and a version
	// that the catalogue no longer recognises would silently lose the timings.
	ytMusicMobile = ytClient{name: "ANDROID_MUSIC", version: ytMusicMobileVersion, agent: ytMusicMobileAgent, id: "21", mobile: true}
)

// YTMusic fetches lyrics from YouTube Music's own catalogue. It only answers for
// YouTube tracks, because a video id is the only handle the catalogue has.
type YTMusic struct {
	// Client is the HTTP client to use. Nil means the default one.
	Client *http.Client
	// Endpoint is the catalogue host, so a test can point it at its own server.
	Endpoint string
	// Version overrides the web client version.
	Version string
}

// NewYTMusic returns a provider using the given client.
func NewYTMusic(client *http.Client) *YTMusic {
	return &YTMusic{Client: client}
}

func (y *YTMusic) Name() string { return "ytmusic" }

// ytMusicVideoID returns the id a track can be asked about, or "" when the track
// is not one the catalogue knows.
func ytMusicVideoID(t *protocol.Track) string {
	if t == nil || t.Info.SourceName != "youtube" {
		return ""
	}
	id := strings.TrimSpace(t.Info.Identifier)
	if !ytVideoIDPattern.MatchString(id) {
		return ""
	}
	return id
}

// Fetch returns the lyrics YouTube Music has for a track, or ErrNotFound when it
// has none. The timed answer is tried first and the plain one is the fallback,
// so a track whose timings the catalogue withholds still gets its words.
func (y *YTMusic) Fetch(ctx context.Context, t *protocol.Track) (*Lyrics, error) {
	id := ytMusicVideoID(t)
	if id == "" {
		return nil, ErrNotFound
	}
	page, err := y.page(ctx, id)
	if err != nil {
		return nil, err
	}
	if page == "" {
		return nil, ErrNotFound
	}
	// A shape that failed is not a reason to stop: the other one is a separate
	// request and may still answer. Only when neither has anything is the
	// failure raised, and the first one is the one worth reporting.
	var last error
	if v, err := y.ask(ctx, ytMusicMobile, page); err == nil {
		if out := ytMusicFromMobile(t, v); out != nil {
			return out, nil
		}
	} else if !errors.Is(err, ErrNotFound) {
		last = err
	}
	if v, err := y.ask(ctx, ytMusicWeb, page); err == nil {
		if out := ytMusicFromWeb(t, v); out != nil {
			return out, nil
		}
	} else if !errors.Is(err, ErrNotFound) && last == nil {
		last = err
	}
	if last != nil {
		return nil, last
	}
	return nil, ErrNotFound
}

// ytNext is the part of a watch-next answer this reads: the tabs, and the page
// the lyrics tab names.
type ytNext struct {
	Contents struct {
		SingleColumnMusicWatchNextResultsRenderer struct {
			TabbedRenderer struct {
				WatchNextTabbedResultsRenderer struct {
					Tabs []struct {
						TabRenderer struct {
							Endpoint struct {
								BrowseEndpoint struct {
									BrowseID string `json:"browseId"`
									Context  struct {
										MusicConfig struct {
											PageType string `json:"pageType"`
										} `json:"browseEndpointContextMusicConfig"`
									} `json:"browseEndpointContextSupportedConfigs"`
								} `json:"browseEndpoint"`
							} `json:"endpoint"`
						} `json:"tabRenderer"`
					} `json:"tabs"`
				} `json:"watchNextTabbedResultsRenderer"`
			} `json:"tabbedRenderer"`
		} `json:"singleColumnMusicWatchNextResultsRenderer"`
	} `json:"contents"`
}

// page returns the lyrics page a track has, or "" when it has none. The words
// are not in this answer, so an empty result is the catalogue saying the track
// has no lyrics rather than a failure.
func (y *YTMusic) page(ctx context.Context, videoID string) (string, error) {
	var v ytNext
	if err := y.call(ctx, ytMusicWeb, "next", map[string]any{"videoId": videoID}, &v); err != nil {
		return "", err
	}
	for _, tab := range v.Contents.SingleColumnMusicWatchNextResultsRenderer.TabbedRenderer.WatchNextTabbedResultsRenderer.Tabs {
		ep := tab.TabRenderer.Endpoint.BrowseEndpoint
		if ep.Context.MusicConfig.PageType == ytMusicLyricsPage {
			return ep.BrowseID, nil
		}
	}
	return "", nil
}

// ytLyrics is the part of a lyrics page this reads. The page is answered in two
// shapes: the web app is given the words as one run inside a description shelf,
// the mobile app as timed lines inside the elements framework.
type ytLyrics struct {
	Contents struct {
		SectionListRenderer struct {
			Contents []struct {
				MusicDescriptionShelfRenderer struct {
					Description struct {
						Runs []ytRun `json:"runs"`
					} `json:"description"`
					Footer struct {
						Runs []ytRun `json:"runs"`
					} `json:"footer"`
				} `json:"musicDescriptionShelfRenderer"`
			} `json:"contents"`
		} `json:"sectionListRenderer"`
		ElementRenderer struct {
			NewElement struct {
				Type struct {
					ComponentType struct {
						Model struct {
							TimedLyricsModel struct {
								LyricsData struct {
									TimedLyricsData []ytTimedLine `json:"timedLyricsData"`
									SourceMessage   string        `json:"sourceMessage"`
								} `json:"lyricsData"`
							} `json:"timedLyricsModel"`
						} `json:"model"`
					} `json:"componentType"`
				} `json:"type"`
			} `json:"newElement"`
		} `json:"elementRenderer"`
	} `json:"contents"`
}

// ytRun is one piece of text in a renderer that splits its message into runs.
type ytRun struct {
	Text string `json:"text"`
}

// ytTimedLine is one line of a timed answer. A line without a cue is the
// catalogue saying it has the words but not when they are sung.
type ytTimedLine struct {
	Line string `json:"lyricLine"`
	Cue  *ytCue `json:"cueRange"`
}

type ytCue struct {
	Start ytMillis `json:"startTimeMilliseconds"`
	End   ytMillis `json:"endTimeMilliseconds"`
}

// ytMillis reads a millisecond count the catalogue sends as a string, and reads
// one sent as a number just as well. A value it cannot read is zero rather than
// an error, because one unreadable timestamp should not cost the whole track its
// lyrics.
type ytMillis int64

func (m *ytMillis) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*m = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*m = 0
		return nil
	}
	*m = ytMillis(v)
	return nil
}

// ask posts one browse request for the lyrics page.
func (y *YTMusic) ask(ctx context.Context, c ytClient, page string) (ytLyrics, error) {
	var v ytLyrics
	if err := y.call(ctx, c, "browse", map[string]any{"browseId": page}, &v); err != nil {
		return ytLyrics{}, err
	}
	return v, nil
}

// call posts one InnerTube request and decodes the answer.
func (y *YTMusic) call(ctx context.Context, c ytClient, endpoint string, body map[string]any, dest any) error {
	ctx, cancel := context.WithTimeout(ctx, ytMusicTimeout)
	defer cancel()
	version := c.version
	if c.name == ytMusicWeb.name {
		if v := strings.TrimSpace(y.Version); v != "" {
			version = v
		}
	}
	body["context"] = map[string]any{"client": ytMusicClientContext(c, version)}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	base := strings.TrimSpace(y.Endpoint)
	if base == "" {
		base = YTMusicEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/youtubei/v1/"+endpoint+"?prettyPrint=false", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("X-YouTube-Client-Name", c.id)
	req.Header.Set("X-YouTube-Client-Version", version)
	client := y.Client
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lyrics: youtube music answered %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ytMusicBody))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("lyrics: youtube music sent an unreadable answer: %w", err)
	}
	return nil
}

// ytMusicClientContext builds the client identity. Only the body carries it, the
// same as the source package: the endpoint needs no API key.
func ytMusicClientContext(c ytClient, version string) map[string]any {
	client := map[string]any{"clientName": c.name, "clientVersion": version, "hl": "en", "gl": "US"}
	if c.mobile {
		client["androidSdkVersion"] = 30
		client["osName"] = "Android"
		client["osVersion"] = "11"
	}
	return client
}

// ytMusicFromMobile builds the payload from the mobile answer. Timed lines are
// returned when every line carries a cue; an answer that has the words without
// the timings is still worth returning as text, because a wall of text beats
// nothing.
func ytMusicFromMobile(t *protocol.Track, v ytLyrics) *Lyrics {
	data := v.Contents.ElementRenderer.NewElement.Type.ComponentType.Model.TimedLyricsModel.LyricsData
	if len(data.TimedLyricsData) == 0 {
		return nil
	}
	out := ytMusicLyrics(t, data.SourceMessage)
	// Every line has to carry a cue for the answer to be the timed one. One line
	// without a cue means the catalogue has the words but not when they are sung,
	// which is still worth returning, as text.
	timed := true
	for _, l := range data.TimedLyricsData {
		if l.Cue == nil {
			timed = false
			break
		}
	}
	if !timed {
		words := make([]string, 0, len(data.TimedLyricsData))
		for _, l := range data.TimedLyricsData {
			words = append(words, l.Line)
		}
		joined := strings.TrimSpace(strings.Join(words, "\n"))
		if joined == "" {
			return nil
		}
		out.Text = &joined
		return out
	}
	out.Lines = make([]Line, 0, len(data.TimedLyricsData))
	for _, l := range data.TimedLyricsData {
		line := Line{Timestamp: int64(l.Cue.Start), Line: l.Line, Plugin: protocol.Object{}}
		if d := int64(l.Cue.End) - int64(l.Cue.Start); d > 0 {
			line.Duration = &d
		}
		out.Lines = append(out.Lines, line)
	}
	return out
}

// ytMusicFromWeb builds the payload from the web answer, which is the whole text
// as one run. An answer with no run is the catalogue saying it has nothing, such
// as the message it sends for a track whose lyrics are not licensed.
func ytMusicFromWeb(t *protocol.Track, v ytLyrics) *Lyrics {
	shelf := v.Contents.SectionListRenderer.Contents
	if len(shelf) == 0 || len(shelf[0].MusicDescriptionShelfRenderer.Description.Runs) == 0 {
		return nil
	}
	text := strings.TrimSpace(shelf[0].MusicDescriptionShelfRenderer.Description.Runs[0].Text)
	if text == "" {
		return nil
	}
	out := ytMusicLyrics(t, ytMusicRunText(shelf[0].MusicDescriptionShelfRenderer.Footer.Runs))
	out.Text = &text
	return out
}

// ytMusicLyrics is the payload both shapes share.
func ytMusicLyrics(t *protocol.Track, source string) *Lyrics {
	return &Lyrics{SourceName: t.Info.SourceName, Provider: "ytmusic", Plugin: ytMusicSource(source)}
}

// ytMusicRunText joins the runs of a shelf footer, which is where the web answer
// names whoever licensed it the words.
func ytMusicRunText(runs []ytRun) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

// ytMusicSource reports who supplied the words. The catalogue credits the
// licensor, and a client showing lyrics should be able to pass that on, so it
// travels in the plugin object rather than being dropped.
func ytMusicSource(message string) protocol.Object {
	message = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(message), "Source:"))
	if message == "" {
		return protocol.Object{}
	}
	return protocol.Object{"source": message}
}
