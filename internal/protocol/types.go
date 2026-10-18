// Package protocol contains the Lavalink v4 wire types.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

type Object = map[string]any

// Int64 is a JSON integer that also accepts a string holding one, and a float
// with no fractional part. Lavalink is Jackson-based and coerces those into an
// int field, so clients have shipped that send "position": "0"; encoding/json
// rejects them with an unmarshal error and the whole request fails.
type Int64 int64

func (n *Int64) UnmarshalJSON(b []byte) error {
	v, err := ParseInt(b)
	if err != nil {
		return err
	}
	*n = Int64(v)
	return nil
}

// ParseInt decodes a JSON integer, a string holding one, or a float with no
// fractional part. A JSON null is rejected so callers can treat it explicitly.
func ParseInt(b []byte) (int64, error) {
	s := string(bytes.TrimSpace(b))
	if len(s) > 1 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return 0, err
		}
		s = strings.TrimSpace(str)
	}
	if s == "" {
		return 0, errors.New("empty number")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) {
		return 0, errors.New("not an integer")
	}
	return int64(f), nil
}

type Track struct {
	Encoded    string    `json:"encoded"`
	Info       TrackInfo `json:"info"`
	PluginInfo Object    `json:"pluginInfo"`
	UserData   Object    `json:"userData"`
}

type TrackInfo struct {
	Identifier string  `json:"identifier"`
	IsSeekable bool    `json:"isSeekable"`
	Author     string  `json:"author"`
	Length     int64   `json:"length"`
	IsStream   bool    `json:"isStream"`
	Position   int64   `json:"position"`
	Title      string  `json:"title"`
	URI        *string `json:"uri"`
	ArtworkURL *string `json:"artworkUrl"`
	ISRC       *string `json:"isrc"`
	SourceName string  `json:"sourceName"`
}

type LoadResult struct {
	LoadType string `json:"loadType"`
	Data     any    `json:"data"`
}
type Playlist struct {
	Info       PlaylistInfo `json:"info"`
	PluginInfo Object       `json:"pluginInfo"`
	Tracks     []*Track     `json:"tracks"`
}
type PlaylistInfo struct {
	Name          string `json:"name"`
	SelectedTrack int    `json:"selectedTrack"`
}
type Exception struct {
	Message         string `json:"message"`
	Severity        string `json:"severity"`
	Cause           string `json:"cause"`
	CauseStackTrace string `json:"causeStackTrace"`
}

func LoadError(err error) LoadResult {
	return LoadResult{"error", Exception{Message: err.Error(), Severity: "common", Cause: "golalink.source", CauseStackTrace: ""}}
}
func String(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func Clone(t *Track) *Track {
	if t == nil {
		return nil
	}
	b, _ := json.Marshal(t)
	var c Track
	_ = json.Unmarshal(b, &c)
	return &c
}

type VoiceState struct {
	Token     string  `json:"token"`
	Endpoint  string  `json:"endpoint"`
	SessionID string  `json:"sessionId"`
	ChannelID *string `json:"channelId"`
}
type PlayerState struct {
	Time      int64 `json:"time"`
	Position  int64 `json:"position"`
	Connected bool  `json:"connected"`
	Ping      int64 `json:"ping"`
}
type Player struct {
	GuildID string      `json:"guildId"`
	Track   *Track      `json:"track"`
	Volume  int         `json:"volume"`
	Paused  bool        `json:"paused"`
	State   PlayerState `json:"state"`
	Voice   VoiceState  `json:"voice"`
	Filters Object      `json:"filters"`
}

// RawMessage distinguishes an omitted field from an explicit null.
type TrackUpdate struct {
	Encoded    json.RawMessage `json:"encoded"`
	Identifier *string         `json:"identifier"`
	UserData   Object          `json:"userData"`
}
type PlayerUpdate struct {
	Track        *TrackUpdate    `json:"track"`
	EncodedTrack json.RawMessage `json:"encodedTrack"`
	Identifier   *string         `json:"identifier"`
	Position     *Int64          `json:"position"`
	EndTime      json.RawMessage `json:"endTime"`
	Volume       *Int64          `json:"volume"`
	Paused       *bool           `json:"paused"`
	Filters      *Object         `json:"filters"`
	Voice        *VoiceState     `json:"voice"`
}
