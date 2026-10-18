package source

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

type Spotify struct {
	refreshToken                                string
	client                                      *http.Client
	clientID, secret, market, baseURL, tokenURL string
	mu                                          sync.Mutex
	token                                       string
	expires                                     time.Time
}
type spAlbum struct {
	Name   string `json:"name"`
	Images []struct {
		URL string `json:"url"`
	} `json:"images"`
}
type spTrack struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Duration int64  `json:"duration_ms"`
	Type     string `json:"type"`
	IsLocal  bool   `json:"is_local"`
	Artists  []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album       spAlbum `json:"album"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
}

func (s *Spotify) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.expires) {
		return s.token, nil
	}
	if s.clientID == "" || s.secret == "" {
		return "", errors.New("Spotify requires SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET")
	}
	form := url.Values{"grant_type": []string{"client_credentials"}}
	if s.refreshToken != "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", s.refreshToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(s.clientID, s.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return "", fmt.Errorf("Spotify authentication returned HTTP %d", r.StatusCode)
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&t); err != nil {
		return "", err
	}
	if t.AccessToken == "" || t.ExpiresIn <= 0 {
		return "", errors.New("invalid Spotify token response")
	}
	s.token = t.AccessToken
	if t.RefreshToken != "" {
		s.refreshToken = t.RefreshToken
	}
	s.expires = time.Now().Add(time.Duration(t.ExpiresIn)*time.Second - 30*time.Second)
	return s.token, nil
}
func (s *Spotify) get(ctx context.Context, endpoint string, dest any) error {
	token, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	r, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode == 401 {
		s.mu.Lock()
		s.expires = time.Time{}
		s.mu.Unlock()
	}
	if r.StatusCode == 429 {
		return fmt.Errorf("Spotify rate limited; retry after %s seconds", r.Header.Get("Retry-After"))
	}
	if r.StatusCode != 200 {
		return fmt.Errorf("Spotify returned HTTP %d (check application permissions and quota mode)", r.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(dest)
}

var spotifyID = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)

func spotifyRef(id string) (string, string, error) {
	var parts []string
	if strings.HasPrefix(id, "spotify:") {
		parts = strings.Split(strings.TrimPrefix(id, "spotify:"), ":")
	} else {
		u, e := url.Parse(id)
		if e != nil || u.Hostname() != "open.spotify.com" {
			return "", "", errors.New("invalid Spotify URL")
		}
		parts = strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) > 0 && strings.HasPrefix(parts[0], "intl-") {
			parts = parts[1:]
		}
	}
	if len(parts) != 2 || !spotifyID.MatchString(parts[1]) || (parts[0] != "track" && parts[0] != "album" && parts[0] != "playlist" && parts[0] != "artist") {
		return "", "", errors.New("expected Spotify track, album, playlist or artist")
	}
	return parts[0], parts[1], nil
}
func spotifyTrack(t spTrack) (*protocol.Track, error) {
	authors := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		authors = append(authors, a.Name)
	}
	art := ""
	if len(t.Album.Images) > 0 {
		art = t.Album.Images[0].URL
	}
	r, err := protocol.Encode(protocol.TrackInfo{Identifier: t.ID, Title: t.Name, Author: strings.Join(authors, ", "), Length: t.Duration, IsSeekable: true, URI: protocol.String("https://open.spotify.com/track/" + t.ID), ArtworkURL: protocol.String(art), ISRC: protocol.String(t.ExternalIDs.ISRC), SourceName: "spotify"})
	if err == nil {
		r.PluginInfo["playbackSource"] = "youtube-search-match"
		r.PluginInfo["albumName"] = t.Album.Name
	}
	return r, err
}

// playlistPage fetches one page of playlist entries. Newer Spotify applications
// only allow /items while older ones only allow /tracks, so a failure on one is
// retried on the other, and both reasons are reported if neither works.
func (s *Spotify) playlistPage(ctx context.Context, kind, key string, limit, offset int, market string, dest any) error {
	page := "?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset) + "&market=" + market
	if kind != "playlist" {
		return s.get(ctx, "/"+kind+"s/"+key+"/tracks"+page, dest)
	}
	itemsErr := s.get(ctx, "/playlists/"+key+"/items"+page, dest)
	if itemsErr == nil {
		return nil
	}
	if tracksErr := s.get(ctx, "/playlists/"+key+"/tracks"+page, dest); tracksErr != nil {
		return fmt.Errorf("playlist items failed (%v); playlist tracks failed (%v)", itemsErr, tracksErr)
	}
	return nil
}

func (s *Spotify) Load(ctx context.Context, id string, searchLimit, maxTracks int) (protocol.LoadResult, error) {
	market := url.QueryEscape(s.market)
	if strings.HasPrefix(id, "spsearch:") {
		var data struct {
			Tracks struct {
				Items []spTrack `json:"items"`
			} `json:"tracks"`
		}
		err := s.get(ctx, "/search?type=track&limit="+strconv.Itoa(min(searchLimit, 10))+"&market="+market+"&q="+url.QueryEscape(strings.TrimPrefix(id, "spsearch:")), &data)
		if err != nil {
			return protocol.LoadResult{}, err
		}
		tracks := []*protocol.Track{}
		for _, t := range data.Tracks.Items {
			v, e := spotifyTrack(t)
			if e != nil {
				return protocol.LoadResult{}, e
			}
			tracks = append(tracks, v)
		}
		if len(tracks) == 0 {
			return protocol.LoadResult{LoadType: "empty", Data: nil}, nil
		}
		return protocol.LoadResult{LoadType: "search", Data: tracks}, nil
	}
	kind, key, err := spotifyRef(id)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	if kind == "track" {
		var t spTrack
		if err = s.get(ctx, "/tracks/"+key+"?market="+market, &t); err != nil {
			return protocol.LoadResult{}, err
		}
		track, e := spotifyTrack(t)
		return protocol.LoadResult{LoadType: "track", Data: track}, e
	}
	// An artist is loaded as its top tracks, the same thing LavaSrc does. The
	// endpoint caps out at ten tracks and requires a market.
	if kind == "artist" {
		var artist struct {
			Name string `json:"name"`
		}
		if err = s.get(ctx, "/artists/"+key, &artist); err != nil {
			return protocol.LoadResult{}, err
		}
		var top struct {
			Tracks []spTrack `json:"tracks"`
		}
		if err = s.get(ctx, "/artists/"+key+"/top-tracks?market="+market, &top); err != nil {
			return protocol.LoadResult{}, err
		}
		tracks := make([]*protocol.Track, 0, len(top.Tracks))
		for _, t := range top.Tracks {
			if t.ID == "" || t.IsLocal || (t.Type != "" && t.Type != "track") {
				continue
			}
			v, e := spotifyTrack(t)
			if e != nil {
				return protocol.LoadResult{}, e
			}
			tracks = append(tracks, v)
			if len(tracks) >= maxTracks {
				break
			}
		}
		if len(tracks) == 0 {
			return protocol.LoadResult{}, errors.New("Spotify artist has no top tracks in this market")
		}
		name := artist.Name
		if name == "" {
			name = "Spotify artist"
		}
		return protocol.LoadResult{LoadType: "playlist", Data: protocol.Playlist{Info: protocol.PlaylistInfo{Name: name + " top tracks", SelectedTrack: -1}, PluginInfo: protocol.Object{"type": "artist", "truncated": len(tracks) >= maxTracks}, Tracks: tracks}}, nil
	}
	// Spotify's own algorithmic playlists are not readable with application
	// credentials. Saying so avoids a confusing 404 from the API.
	if kind == "playlist" && strings.HasPrefix(key, "37i9dQZ") && s.refreshToken == "" {
		return protocol.LoadResult{}, errors.New("Spotify generated playlists need a user refresh token; application credentials cannot read them")
	}
	var meta spAlbum
	if err = s.get(ctx, "/"+kind+"s/"+key+"?market="+market, &meta); err != nil {
		return protocol.LoadResult{}, err
	}
	tracks := []*protocol.Track{}
	offset := 0
	truncated := false
	for len(tracks) < maxTracks {
		limit := min(50, maxTracks-len(tracks))
		var page struct {
			Items []json.RawMessage `json:"items"`
			Next  *string           `json:"next"`
		}
		if err = s.playlistPage(ctx, kind, key, limit, offset, market, &page); err != nil {
			return protocol.LoadResult{}, err
		}
		for _, raw := range page.Items {
			var t spTrack
			if kind == "playlist" {
				var item struct {
					Track *spTrack `json:"track"`
					Item  *spTrack `json:"item"`
				}
				if err = json.Unmarshal(raw, &item); err != nil {
					return protocol.LoadResult{}, err
				}
				if item.Item != nil {
					t = *item.Item
				} else if item.Track != nil {
					t = *item.Track
				} else {
					continue
				}
			} else {
				if err = json.Unmarshal(raw, &t); err != nil {
					return protocol.LoadResult{}, err
				}
				t.Album = meta
			}
			if t.ID == "" || t.IsLocal || (t.Type != "" && t.Type != "track") {
				continue
			}
			v, e := spotifyTrack(t)
			if e != nil {
				return protocol.LoadResult{}, e
			}
			tracks = append(tracks, v)
		}
		if page.Next == nil || len(page.Items) == 0 {
			break
		}
		offset += len(page.Items)
		truncated = len(tracks) >= maxTracks
		if offset >= maxTracks*3 {
			truncated = true
			break
		}
	}
	return protocol.LoadResult{LoadType: "playlist", Data: protocol.Playlist{Info: protocol.PlaylistInfo{Name: meta.Name, SelectedTrack: -1}, PluginInfo: protocol.Object{"truncated": truncated}, Tracks: tracks}}, nil
}
