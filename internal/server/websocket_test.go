package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golalink/internal/config"
)

func websocketHeaders() http.Header {
	return http.Header{"Authorization": {"secret"}, "User-Id": {"1"}, "Client-Name": {"test/1"}}
}

func TestFailedWebSocketResumeKeepsOriginalDeadline(t *testing.T) {
	srv := newTestServer(t, nil)
	disconnected := time.Now().Add(-40 * time.Second)
	v := &session{
		id: "resume-test", user: "1", resuming: true, timeout: 60, disconnected: disconnected,
		events: make(chan []byte, 1), done: make(chan struct{}), server: srv,
	}
	srv.mu.Lock()
	srv.sessions[v.id] = v
	srv.mu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/v4/websocket", nil)
	req.Header = websocketHeaders()
	req.Header.Set("Session-Id", v.id)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("failed handshake got %d, want 400", rec.Code)
	}
	v.mu.Lock()
	got, attaching, connected := v.disconnected, v.attaching, v.connected
	v.mu.Unlock()
	if !got.Equal(disconnected) || attaching || connected {
		t.Fatalf("failed upgrade changed disconnect time to %v or connection state to %v/%v", got, attaching, connected)
	}
	if rec := do(srv, http.MethodGet, "/v4/sessions/"+v.id+"/players", "secret"); rec.Code != http.StatusNotFound {
		t.Fatalf("disconnected session got HTTP %d over REST, want 404", rec.Code)
	}

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http")+"/v4/websocket", req.Header)
	if err != nil {
		t.Fatalf("valid resume after failure: %v", err)
	}
	defer ws.Close()
	if resp.Header.Get("Session-Id") != v.id || resp.Header.Get("Session-Resumed") != "true" {
		t.Fatalf("handshake headers did not resume the original session: %v", resp.Header)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ready struct {
		Op      string `json:"op"`
		Resumed bool   `json:"resumed"`
	}
	if err := ws.ReadJSON(&ready); err != nil || ready.Op != "ready" || !ready.Resumed {
		t.Fatalf("resume ready = %+v, err = %v", ready, err)
	}
}

func TestConcurrentWebSocketSessions(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) { c.RateLimits.Enable = false })
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	addr := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/v4/websocket"
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			ws, _, err := websocket.DefaultDialer.Dial(addr, websocketHeaders())
			if err != nil {
				t.Errorf("websocket handshake: %v", err)
				return
			}
			ws.Close()
		})
	}
	wg.Wait()
}
