package source

// Deezer is a metadata-only mirror source.
//
// LavaSrc's Deezer source needs an ARL cookie and a master decryption key,
// because it streams the audio from Deezer itself. Golalink deliberately does
// not do that. Instead this source reads the public metadata endpoints, which
// need no credentials, and playback is mirrored to YouTube exactly like the
// Spotify source. Nothing is downloaded or decrypted from Deezer.
//
// Verified 2026-10-07: /track/{id}, /album/{id} and /playlist/{id} answer
// without credentials, while public track search returns {"data":[],...} with a
// non-zero total, so dzsearch: cannot work without an account.

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

	"golalink/internal/protocol"
)

type Deezer struct {
	client  *http.Client
	baseURL string
}

// dzID is Deezer's numeric resource id.
var dzID = regexp.MustCompile(`^[0-9]{1,20}$`)

type dzArtist struct {
	Name string `json:"name"`
}

type dzCover struct {
	Title string `json:"title"`
	Cover string `json:"cover_medium"`
}

type dzTrack struct {
	ID       int64    `json:"id"`
	Readable bool     `json:"readable"`
	Title    string   `json:"title"`
	Duration int64    `json:"duration"`
	ISRC     string   `json:"isrc"`
	Artist   dzArtist `json:"artist"`
	Album    dzCover  `json:"album"`
}

// dzRef splits a Deezer track, album or playlist reference into its kind and id.
// Bare ids are rejected: a number alone says nothing about which kind it is.
func dzRef(id string) (string, string, error) {
	parts := []string{}
	switch {
	case strings.HasPrefix(id, "deezer:"):
		parts = strings.Split(strings.TrimPrefix(id, "deezer:"), ":")
	case strings.HasPrefix(id, "dzsearch:") || strings.HasPrefix(id, "dzisrc:"):
		return "", "", errors.New("Deezer search and ISRC lookups need a Deezer account; the public search API returns no results without one, so use a deezer.com track, album or playlist link")
	default:
		u, err := url.Parse(id)
		if err != nil {
			return "", "", errors.New("invalid Deezer URL")
		}
		host := strings.ToLower(u.Hostname())
		if host != "deezer.com" && !strings.HasSuffix(host, ".deezer.com") {
			return "", "", errors.New("invalid Deezer URL")
		}
		// Links may carry a country segment, as in /us/album/302127.
		for _, segment := range strings.Split(strings.Trim(u.Path, "/"), "/") {
			if segment != "" {
				parts = append(parts, segment)
			}
		}
		if len(parts) > 0 && len(parts[0]) == 2 {
			parts = parts[1:]
		}
	}
	if len(parts) != 2 || !dzID.MatchString(parts[1]) {
		return "", "", errors.New("expected a Deezer track, album or playlist link")
	}
	kind := strings.TrimSuffix(parts[0], "s")
	if kind != "track" && kind != "album" && kind != "playlist" {
		return "", "", errors.New("expected a Deezer track, album or playlist link")
	}
	return kind, parts[1], nil
}

func dzTrackInfo(t dzTrack) (protocol.TrackInfo, error) {
	if t.ID == 0 || t.Title == "" {
		return protocol.TrackInfo{}, errors.New("Deezer returned an incomplete track")
	}
	id := strconv.FormatInt(t.ID, 10)
	artist := strings.TrimSpace(t.Artist.Name)
	if artist == "" {
		artist = "Unknown artist"
	}
	info := protocol.TrackInfo{
		Identifier: id,
		Title:      t.Title,
		Author:     artist,
		Length:     t.Duration * 1000,
		IsSeekable: true,
		URI:        protocol.String("https://www.deezer.com/track/" + id),
		SourceName: "deezer",
	}
	if t.ISRC != "" {
		info.ISRC = protocol.String(t.ISRC)
	}
	if t.Album.Cover != "" {
		info.ArtworkURL = protocol.String(t.Album.Cover)
	}
	return info, nil
}

// dzEncode builds the track and records where playback will come from, so a
// client can see that the audio is mirrored rather than served by Deezer.
func dzEncode(t dzTrack, kind, album string) (*protocol.Track, error) {
	info, err := dzTrackInfo(t)
	if err != nil {
		return nil, err
	}
	v, err := protocol.Encode(info)
	if err != nil {
		return nil, err
	}
	v.PluginInfo["playbackSource"] = "youtube-search-match"
	if t.Album.Title != "" {
		album = t.Album.Title
	}
	if album != "" {
		v.PluginInfo["albumName"] = album
	}
	if kind != "" {
		v.PluginInfo["deezerType"] = kind
	}
	return v, nil
}

// dzGet reads one public Deezer endpoint.
func (d *Deezer) dzGet(ctx context.Context, path string, dest any) error {
	raw := strings.TrimSuffix(d.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	r, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusNotFound {
		return errors.New("Deezer resource not found")
	}
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("Deezer returned HTTP %d", r.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(dest); err != nil {
		return err
	}
	return nil
}

// dzPage covers both shapes Deezer uses for a list of tracks: playlists answer
// with a flat {data,next}, while albums nest the same fields under tracks.
type dzPage struct {
	Title string    `json:"title"`
	Data  []dzTrack `json:"data"`
	Next  *string   `json:"next"`
	Sub   struct {
		Data []dzTrack `json:"data"`
		Next *string   `json:"next"`
	} `json:"tracks"`
}

// entries returns whichever list shape the response used.
func (p *dzPage) entries() []dzTrack {
	if len(p.Data) == 0 && len(p.Sub.Data) > 0 {
		return p.Sub.Data
	}
	return p.Data
}

// nextLink returns the paging link from either shape.
func (p *dzPage) nextLink() *string {
	if p.Next != nil {
		return p.Next
	}
	return p.Sub.Next
}

// dzTracks follows the next links until maxTracks entries are collected.
// Following an absolute next link is safe because the link is checked against
// the host this source was configured with, and netguard rejects anything that
// is not a public address.
func (d *Deezer) dzTracks(ctx context.Context, pageURL string, maxTracks int) ([]dzTrack, bool, error) {
	if maxTracks <= 0 {
		return nil, true, nil
	}
	base, err := url.Parse(d.baseURL)
	if err != nil {
		return nil, false, err
	}
	allowed := strings.ToLower(base.Hostname())
	all := []dzTrack{}
	next := pageURL
	for next != "" && len(all) < maxTracks {
		var page dzPage
		if strings.HasPrefix(next, "http") {
			u, err := url.Parse(next)
			if err != nil {
				return nil, false, err
			}
			if host := strings.ToLower(u.Hostname()); host != allowed {
				return nil, false, fmt.Errorf("unexpected Deezer paging host %q", u.Hostname())
			}
			if err = d.dzGet(ctx, u.RequestURI(), &page); err != nil {
				return nil, false, err
			}
		} else if err := d.dzGet(ctx, next, &page); err != nil {
			return nil, false, err
		}
		entries := page.entries()
		all = append(all, entries...)
		if page.nextLink() == nil || len(entries) == 0 {
			break
		}
		next = *page.nextLink()
	}
	truncated := len(all) > maxTracks
	if truncated {
		all = all[:maxTracks]
	}
	return all, truncated, nil
}

// Load reads Deezer metadata and returns tracks that mirror to YouTube.
func (d *Deezer) Load(ctx context.Context, id string, _ int, maxTracks int) (protocol.LoadResult, error) {
	kind, key, err := dzRef(id)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	if kind == "track" {
		var t dzTrack
		if err = d.dzGet(ctx, "/track/"+key, &t); err != nil {
			return protocol.LoadResult{}, err
		}
		track, err := dzEncode(t, "", "")
		if err != nil {
			return protocol.LoadResult{}, err
		}
		return protocol.LoadResult{LoadType: "track", Data: track}, nil
	}
	var meta dzPage
	if err = d.dzGet(ctx, "/"+kind+"/"+key, &meta); err != nil {
		return protocol.LoadResult{}, err
	}
	raw := meta.entries()
	truncated := false
	// The resource endpoint already carries the first page, so the continuation
	// is appended to it rather than replacing it.
	if next := meta.nextLink(); next != nil && len(raw) < maxTracks {
		more, moreTruncated, err := d.dzTracks(ctx, *next, maxTracks-len(raw))
		if err != nil {
			return protocol.LoadResult{}, err
		}
		raw = append(raw, more...)
		truncated = moreTruncated
	} else if len(raw) > maxTracks {
		raw, truncated = raw[:maxTracks], true
	}
	tracks := make([]*protocol.Track, 0, len(raw))
	for _, t := range raw {
		v, err := dzEncode(t, kind, meta.Title)
		if err != nil {
			continue // Deezer lists some entries without a playable track
		}
		tracks = append(tracks, v)
	}
	if len(tracks) == 0 {
		return protocol.LoadResult{}, errors.New("Deezer returned no usable tracks")
	}
	name := meta.Title
	if name == "" {
		name = "Deezer " + kind
	}
	return protocol.LoadResult{LoadType: "playlist", Data: protocol.Playlist{Info: protocol.PlaylistInfo{Name: name, SelectedTrack: -1}, PluginInfo: protocol.Object{"type": kind, "truncated": truncated}, Tracks: tracks}}, nil
}
