package source

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"unicode"

	"golalink/internal/process"
	"golalink/internal/protocol"
)

type ytMedia struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Uploader    string            `json:"uploader"`
	Artist      string            `json:"artist"`
	Channel     string            `json:"channel"`
	Duration    float64           `json:"duration"`
	IsLive      bool              `json:"is_live"`
	URL         string            `json:"url"`
	WebpageURL  string            `json:"webpage_url"`
	Thumbnail   string            `json:"thumbnail"`
	Extractor   string            `json:"extractor_key"`
	Entries     []ytMedia         `json:"entries"`
	Type        string            `json:"_type"`
	HTTPHeaders map[string]string `json:"http_headers"`
}

func (s *Service) ytdlpArgs() []string {
	args := []string{"--ignore-config", "--no-warnings", "--no-progress", "--skip-download", "--socket-timeout", "15", "--retries", "1", "--extractor-retries", "1", "--playlist-end", strconv.Itoa(s.Config.MaxPlaylistTracks)}
	if _, err := exec.LookPath("node"); err == nil {
		args = append(args, "--js-runtimes", "node")
	}
	return args
}
func normalizeID(id string, n int) string {
	if youtubeID.MatchString(id) {
		return "https://www.youtube.com/watch?v=" + id
	}
	for _, p := range []string{"ytsearch:", "ytmsearch:", "scsearch:"} {
		if strings.HasPrefix(id, p) {
			target := strings.TrimSuffix(p, ":")
			if p == "ytmsearch:" {
				target = "ytsearch"
			}
			return target + strconv.Itoa(n) + ":" + strings.TrimPrefix(id, p)
		}
	}
	return id
}
func (s *Service) loadYTDLP(ctx context.Context, id string) (protocol.LoadResult, error) {
	if !s.Config.YTDLPEnabled {
		return protocol.LoadResult{}, errors.New("yt-dlp is disabled (ytDlpEnabled: false)")
	}
	b, err := process.Run(ctx, s.Config.YTDLP, nil, 16<<20, append(s.ytdlpArgs(), "--flat-playlist", "--dump-single-json", "--", normalizeID(id, s.Config.SearchResults))...)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	var m ytMedia
	if err = json.Unmarshal(b, &m); err != nil {
		return protocol.LoadResult{}, err
	}
	kind := sourceKind(id)
	search := strings.HasPrefix(id, "ytsearch:") || strings.HasPrefix(id, "ytmsearch:") || strings.HasPrefix(id, "scsearch:")
	if m.Type != "playlist" && m.Entries == nil {
		t, e := mediaTrack(m, kind)
		return protocol.LoadResult{LoadType: "track", Data: t}, e
	}
	tracks := make([]*protocol.Track, 0, len(m.Entries))
	for _, entry := range m.Entries {
		if entry.ID == "" || entry.Title == "" {
			continue
		}
		t, e := mediaTrack(entry, kind)
		if e != nil {
			return protocol.LoadResult{}, e
		}
		tracks = append(tracks, t)
	}
	if len(tracks) == 0 {
		return protocol.LoadResult{LoadType: "empty", Data: nil}, nil
	}
	if search {
		return protocol.LoadResult{LoadType: "search", Data: tracks}, nil
	}
	return protocol.LoadResult{LoadType: "playlist", Data: protocol.Playlist{Info: protocol.PlaylistInfo{Name: m.Title, SelectedTrack: -1}, PluginInfo: protocol.Object{}, Tracks: tracks}}, nil
}
func mediaTrack(m ytMedia, kind string) (*protocol.Track, error) {
	u := m.WebpageURL
	if u == "" {
		u = m.URL
	}
	if kind == "youtube" {
		u = "https://www.youtube.com/watch?v=" + m.ID
	}
	author := m.Artist
	if author == "" {
		author = m.Uploader
	}
	if author == "" {
		author = m.Channel
	}
	if author == "" {
		author = "Unknown artist"
	}
	return protocol.Encode(protocol.TrackInfo{Identifier: m.ID, Title: m.Title, Author: author, Length: int64(m.Duration * 1000), IsStream: m.IsLive, IsSeekable: !m.IsLive, URI: protocol.String(u), ArtworkURL: protocol.String(m.Thumbnail), SourceName: kind})
}
func tokens(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 1 {
			m[w] = true
		}
	}
	return m
}
func overlap(want string, got string) float64 {
	a, b := tokens(want), tokens(got)
	if len(a) == 0 {
		return 0
	}
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}
func matchTrack(want *protocol.Track, tracks []*protocol.Track) *protocol.Track {
	var best *protocol.Track
	bestScore := 0.0
	for _, t := range tracks {
		if t.Info.IsStream {
			continue
		}
		title := overlap(want.Info.Title, t.Info.Title)
		artist := overlap(want.Info.Author, t.Info.Title+" "+t.Info.Author)
		if title < 0.6 || artist < 0.4 {
			continue
		}
		delta := math.Abs(float64(t.Info.Length - want.Info.Length))
		if want.Info.Length > 0 && (t.Info.Length <= 0 || delta > math.Max(15000, float64(want.Info.Length)*0.12)) {
			continue
		}
		score := title + artist
		if want.Info.Length > 0 {
			score += 1 - delta/math.Max(1, float64(want.Info.Length))
		}
		if score > bestScore {
			bestScore = score
			best = t
		}
	}
	return best
}
