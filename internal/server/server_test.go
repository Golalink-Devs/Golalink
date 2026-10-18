package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"golalink/internal/audio"
	"golalink/internal/config"
	"golalink/internal/protocol"
	"golalink/internal/voice"
)

// These tests go through Handler rather than the handlers themselves, because
// what a client sees is the mux: the identity headers, the limiter, the password
// check, and the order they run in. A handler called directly would skip all of
// that and pass while the node was unreachable.

// The three dependencies a node cannot start without. None of them is reached by
// a route that only reads state, so each refuses loudly if a test drifts into
// playback.
type stubLoader struct{}

func (stubLoader) Load(context.Context, string) (protocol.LoadResult, error) {
	return protocol.LoadResult{}, errors.New("the loader is not part of this test")
}

type stubEngine struct{}

func (stubEngine) Start(context.Context, *protocol.Track, int64, int, protocol.Object) (audio.Frames, error) {
	return nil, errors.New("the engine is not part of this test")
}

type stubVoice struct{}

func (stubVoice) Connect(context.Context, string, string, protocol.VoiceState, func(voice.Closed)) (voice.Connection, error) {
	return nil, errors.New("the voice factory is not part of this test")
}

// newTestServer builds a node whose helpers are off, so what it can play is
// decided by the configuration alone.
func newTestServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	c := config.Default()
	c.Password = "secret"
	c.AutoDownload = false
	c.InProcessAudio = false
	c.YTDLPEnabled = false
	if mutate != nil {
		mutate(&c)
	}
	srv, err := New(c, Dependencies{
		Loader: stubLoader{},
		Engine: stubEngine{},
		Voice:  stubVoice{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// do sends one request with the password a client would send, which is the
// header value itself and nothing else.
func do(srv *Server, method, path, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if password != "" {
		req.Header.Set("Authorization", password)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// errorBody is the shape every rejection uses, which is the one Lavalink clients
// parse.
type errorBody struct {
	Status  int    `json:"status"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestRequestsWithoutThePasswordAreRejected(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, tc := range []struct{ name, password string }{
		{"no header", ""},
		{"empty header", " "},
		{"wrong password", "not-the-password"},
		{"password with a prefix", "xsecret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(srv, http.MethodGet, "/v4/info", tc.password)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			body := decodeError(t, rec)
			if body.Status != http.StatusUnauthorized || body.Error != "Unauthorized" {
				t.Errorf("body = %+v, want a 401 error", body)
			}
			if body.Message != "invalid authorization" {
				t.Errorf("message = %q, want the authorization message", body.Message)
			}
			if body.Path != "/v4/info" {
				t.Errorf("path = %q, want the request path", body.Path)
			}
		})
	}

	if rec := do(srv, http.MethodGet, "/v4/info", "secret"); rec.Code != http.StatusOK {
		t.Errorf("the configured password was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// TestResponsesCarryTheNodeIdentity pins the two headers a client uses to tell a
// v4 node from a v3 one and to know what answered. They are set before the mux
// runs, so a rejection carries them too.
func TestResponsesCarryTheNodeIdentity(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, tc := range []struct {
		name     string
		password string
		status   int
	}{
		{"accepted", "secret", http.StatusOK},
		{"rejected", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(srv, http.MethodGet, "/v4/info", tc.password)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get("Lavalink-Api-Version"); got != "4" {
				t.Errorf("Lavalink-Api-Version = %q, want 4", got)
			}
			if got := rec.Header().Get("Server"); got != "Golalink/"+Version {
				t.Errorf("Server = %q, want Golalink/%s", got, Version)
			}
		})
	}
}

// TestVersionIsPublicAndUnprotected keeps the health probe a client uses before
// it authenticates.
func TestVersionIsPublic(t *testing.T) {
	rec := do(newTestServer(t, nil), http.MethodGet, "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a password", rec.Code)
	}
	if got := rec.Body.String(); got != APIVersion {
		t.Errorf("body = %q, want %q", got, APIVersion)
	}
}

// TestInfoReportsWhatTheNodeCanPlay covers the one field clients read to decide
// whether a prefix is worth sending here: the native clients count even without
// yt-dlp, and yt-dlp counts for the sources it alone backs.
func TestInfoReportsWhatTheNodeCanPlay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		want   []string
	}{
		{
			name:   "native clients only",
			mutate: func(c *config.Config) { c.YTDLPEnabled = false },
			want:   []string{"http", "youtube", "soundcloud", "deezer"},
		},
		{
			name: "yt-dlp stands in for the natives it replaces",
			mutate: func(c *config.Config) {
				c.YoutubeNative = false
				c.YTDLPEnabled = true
			},
			want: []string{"http", "soundcloud", "youtube", "yt-dlp", "deezer"},
		},
		{
			name: "a spotify client adds its manager",
			mutate: func(c *config.Config) {
				c.SpotifyClientID = "id"
				c.SpotifyClientSecret = "secret"
			},
			want: []string{"http", "youtube", "soundcloud", "spotify", "deezer"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(newTestServer(t, tc.mutate), http.MethodGet, "/v4/info", "secret")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var info struct {
				Version struct {
					Semver string `json:"semver"`
					Major  int    `json:"major"`
					Build  string `json:"build"`
				} `json:"version"`
				SourceManagers []string `json:"sourceManagers"`
				Filters        []string `json:"filters"`
				Plugins        []any    `json:"plugins"`
				Golalink       struct {
					APIVersion string `json:"apiCompatibility"`
				} `json:"golalink"`
				JVM string `json:"jvm"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
				t.Fatalf("decode info: %v", err)
			}
			if !reflect.DeepEqual(info.SourceManagers, tc.want) {
				t.Errorf("sourceManagers = %v, want %v", info.SourceManagers, tc.want)
			}
			// A client refuses to talk to a node that reports another API.
			if info.Version.Major != 4 || info.Golalink.APIVersion != "4.2" {
				t.Errorf("api = %+v / %q, want major 4 and 4.2", info.Version, info.Golalink.APIVersion)
			}
			if !strings.HasPrefix(info.Version.Semver, APIVersion+"+golalink.") {
				t.Errorf("semver = %q, want it to start with %s", info.Version.Semver, APIVersion)
			}
			if len(info.Plugins) != 0 {
				t.Errorf("plugins = %v, want an empty array so a client sees no plugin", info.Plugins)
			}
			if len(info.Filters) == 0 {
				t.Error("filters is empty; a client would think the node has none")
			}
		})
	}
}

func TestUnknownRoutesAreNotFound(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, path := range []string{"/v4/not-a-route", "/v3/loadtracks"} {
		rec := do(srv, http.MethodGet, path, "secret")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

// TestSessionRoutesRejectAnUnknownSession pins that a client resuming a session
// the node has forgotten is told so, instead of being handed an empty player
// list that looks like a working node.
func TestSessionRoutesRejectAnUnknownSession(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v4/sessions/unknown/players"},
		{http.MethodGet, "/v4/sessions/unknown/players/1"},
		{http.MethodPatch, "/v4/sessions/unknown"},
		{http.MethodDelete, "/v4/sessions/unknown/players/1"},
	} {
		rec := do(srv, tc.method, tc.path, "secret")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", tc.method, tc.path, rec.Code)
		}
		if body := decodeError(t, rec); body.Message != "session not found" {
			t.Errorf("%s %s: message = %q, want session not found", tc.method, tc.path, body.Message)
		}
	}
}

// TestRateLimitRefusesABurst pins the limiter that protects a shared node: the
// route allows 2 requests a second with a burst of 5, so the sixth in a row is
// refused rather than queued.
func TestRateLimitRefusesABurst(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) { c.RateLimits.Enable = true })
	for i := 0; i < 5; i++ {
		if rec := do(srv, http.MethodGet, "/version", ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 within the burst", i+1, rec.Code)
		}
	}
	rec := do(srv, http.MethodGet, "/version", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the burst is spent", rec.Code)
	}
	if body := decodeError(t, rec); body.Message != "too many requests" {
		t.Errorf("message = %q, want too many requests", body.Message)
	}

	t.Run("disabled", func(t *testing.T) {
		// A node that turned the limiter off must not refuse a client that opens
		// many players at once.
		srv := newTestServer(t, func(c *config.Config) { c.RateLimits.Enable = false })
		for i := 0; i < 20; i++ {
			if rec := do(srv, http.MethodGet, "/version", ""); rec.Code != http.StatusOK {
				t.Fatalf("request %d: status = %d, want 200 with the limiter off", i+1, rec.Code)
			}
		}
	})
}

// TestMetricsArePrometheusText covers the scrape endpoint: an operator's
// dashboard breaks on a malformed exposition rather than on a status code.
func TestMetricsArePrometheusText(t *testing.T) {
	rec := do(newTestServer(t, nil), http.MethodGet, "/metrics", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"# TYPE golalink_players gauge", "golalink_sessions", "# TYPE golalink_http_requests_total counter"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body is missing %q", want)
		}
	}
}
