package server

import (
	"errors"
	"net/http"

	"golalink/internal/lyrics"
	"golalink/internal/protocol"
)

// The two lyrics routes follow the LavaLyrics plugin, which is what a client
// that already shows lyrics expects: the same paths, the same payload, no
// content when there is nothing to send, and a 404 only when the question itself
// could not be answered.

// lyricsForPlayer answers for the track a player is on. A guild with no player or
// no track is a 404, which is what the plugin sends.
func (s *Server) lyricsForPlayer(w http.ResponseWriter, r *http.Request) {
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
	t := p.Snapshot().Track
	if t == nil {
		s.fail(w, r, 404, "no track is playing")
		return
	}
	s.answerLyrics(w, r, t)
}

// lyricsForEncodedTrack answers for a track the client sends in full, so lyrics
// can be fetched before anything is played.
func (s *Server) lyricsForEncodedTrack(w http.ResponseWriter, r *http.Request) {
	encoded := r.URL.Query().Get("track")
	if encoded == "" {
		s.fail(w, r, 400, "track is required")
		return
	}
	t, err := protocol.Decode(encoded)
	if err != nil {
		s.fail(w, r, 400, err.Error())
		return
	}
	s.answerLyrics(w, r, t)
}

// answerLyrics sends the payload, or no content when there is nothing to send.
// A provider that failed is a bad gateway rather than no content, because the
// difference matters to a client deciding whether to ask again.
func (s *Server) answerLyrics(w http.ResponseWriter, r *http.Request, t *protocol.Track) {
	if !s.lyrics.Enabled() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	found, err := s.lyrics.Fetch(r.Context(), t)
	switch {
	case err == nil:
		s.json(w, http.StatusOK, found)
	case errors.Is(err, lyrics.ErrNotFound):
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, r, http.StatusBadGateway, "lyrics lookup failed: "+err.Error())
	}
}
