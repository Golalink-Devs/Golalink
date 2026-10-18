package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golalink/internal/netguard"
	"golalink/internal/protocol"
)

// Native YouTube resolution through the InnerTube API, so the common paths do
// not depend on an external yt-dlp process.
//
// Two behaviours of the upstream players are deliberately copied because they
// are what makes this work without a JavaScript runtime:
//
//   - Client identities are sent only inside the request body's context.client.
//     The endpoints need no API key.
//   - The clients below are ones that hand out ready-to-play URLs, so no player
//     script has to be fetched and no signature or n-parameter has to be
//     deciphered. A format that still arrives as a signatureCipher is not
//     playable here and is skipped rather than guessed at.
//
// Client versions are the ones yt-dlp 2026.08.19 sends (Unlicense); they age
// quickly, hence Config.YoutubeClientVersion.
type itClient struct {
	Name        string
	Version     string
	UserAgent   string
	SDK         int
	DeviceMake  string
	DeviceModel string
	OSName      string
	OSVersion   string
}

// Client order is set by what actually answers, verified against the live API
// on 2026-10-07: ANDROID returns a progressive format, ANDROID_VR answers
// LOGIN_REQUIRED, and IOS answers with SABR-only formats that carry no URL at
// all. The blocked clients stay in the list because this changes often, and each
// one is only tried when the previous ones fail.
var itClientDefaults = []itClient{
	{
		Name:      "ANDROID",
		Version:   "21.26.364",
		UserAgent: "com.google.android.youtube/21.26.364 (Linux; U; Android 11) gzip",
		SDK:       30,
		OSName:    "Android",
		OSVersion: "11",
	},
	{
		Name:        "ANDROID_VR",
		Version:     "1.65.10",
		UserAgent:   "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
		SDK:         32,
		DeviceMake:  "Oculus",
		DeviceModel: "Quest 3",
		OSName:      "Android",
		OSVersion:   "12L",
	},
	{
		Name:        "IOS",
		Version:     "21.26.4",
		UserAgent:   "com.google.ios.youtube/21.26.4 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)",
		DeviceMake:  "Apple",
		DeviceModel: "iPhone16,2",
		OSName:      "iPhone",
		OSVersion:   "18.3.2.22D82",
	},
}

// searchParams restricts a search to videos. Without it YouTube mixes in
// channels, playlists and shorts, which have no length and are dropped anyway.
const itSearchParams = "EgIQAfABAQ=="

// itMusicSongsParams restricts a YouTube Music search to songs.
const itMusicSongsParams = "EgWKAQIIAWoKEAkQBRAKEAMQBA=="

// itDurationWord matches the written durations YouTube Music returns.
var itDurationWord = regexp.MustCompile(`(\d+)\s*(hour|minute|second)`)

// itMusicClient is the YouTube Music web client. It needs no player script
// because search results carry plain ids.
var itMusicClient = itClient{
	Name:      "WEB_REMIX",
	Version:   "1.20250101.01.00",
	UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
}

// itMaxBody bounds an API response; playlist and search payloads are large.
const itMaxBody = 8 << 20

func (s *Service) itClients() []itClient {
	clients := make([]itClient, len(itClientDefaults))
	copy(clients, itClientDefaults)
	if v := strings.TrimSpace(s.Config.YoutubeClientVersion); v != "" {
		clients[0].Version = v
	}
	return clients
}

// itContext builds the client identity.
func (s *Service) itContext(c itClient, version string) map[string]any {
	client := map[string]any{"clientName": c.Name, "clientVersion": version, "hl": "en", "gl": "US"}
	if c.SDK > 0 {
		client["androidSdkVersion"] = c.SDK
	}
	if c.DeviceMake != "" {
		client["deviceMake"] = c.DeviceMake
	}
	if c.DeviceModel != "" {
		client["deviceModel"] = c.DeviceModel
	}
	if c.OSName != "" {
		client["osName"] = c.OSName
	}
	if c.OSVersion != "" {
		client["osVersion"] = c.OSVersion
	}
	return map[string]any{"client": client}
}

func (s *Service) itCall(ctx context.Context, c itClient, version string, endpoint string, body map[string]any) ([]byte, error) {
	return s.itCallHost(ctx, s.Config.YoutubeEndpoint, c, version, endpoint, body)
}

// itCallHost posts to an InnerTube host. YouTube Music is a separate host with
// its own client identity, so the base has to be selectable.
func (s *Service) itCallHost(ctx context.Context, base string, c itClient, version string, endpoint string, body map[string]any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSuffix(base, "/") + "/youtubei/v1/" + endpoint + "?prettyPrint=false"
	if err = netguard.ValidateURL(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &mediaError{Status: resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, itMaxBody))
}

type itFormat struct {
	Itag            int    `json:"itag"`
	URL             string `json:"url"`
	MimeType        string `json:"mimeType"`
	Bitrate         int    `json:"bitrate"`
	ContentLength   string `json:"contentLength"`
	SignatureCipher string `json:"signatureCipher"`
	Cipher          string `json:"cipher"`
	AudioChannels   int    `json:"audioChannels"`
	AudioQuality    string `json:"audioQuality"`
}

type itPlayer struct {
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	VideoDetails struct {
		VideoID       string `json:"videoId"`
		Title         string `json:"title"`
		Author        string `json:"author"`
		LengthSeconds string `json:"lengthSeconds"`
		IsLiveContent bool   `json:"isLiveContent"`
		Thumbnail     struct {
			Thumbnails []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"thumbnails"`
		} `json:"thumbnail"`
	} `json:"videoDetails"`
	StreamingData struct {
		Formats         []itFormat `json:"formats"`
		AdaptiveFormats []itFormat `json:"adaptiveFormats"`
	} `json:"streamingData"`
}

// pickFormat chooses the best playable format. Selection follows the upstream
// ordering rather than a fixed itag list, but the reality verified on
// 2026-10-07 is that audio-only adaptive formats now arrive SABR-only, so the
// muxed progressive format (itag 18) is what keeps native playback working.
// FFmpeg is asked for 0:a:0, so a muxed stream is decoded as audio normally.
func pickFormat(p itPlayer) (itFormat, bool) {
	best, found := itFormat{}, false
	bestScore := -1
	for _, f := range append(append([]itFormat{}, p.StreamingData.AdaptiveFormats...), p.StreamingData.Formats...) {
		if f.URL == "" || f.SignatureCipher != "" || f.Cipher != "" {
			continue // needs a player script; another client or yt-dlp handles it
		}
		mime := strings.ToLower(f.MimeType)
		audioOnly := strings.HasPrefix(mime, "audio/")
		if !audioOnly && !itHasAudioCodec(mime) {
			continue // video-only
		}
		score := 1
		switch {
		case audioOnly && strings.Contains(mime, "opus"):
			score = 4
		case audioOnly && strings.Contains(mime, "mp4"):
			score = 3
		case audioOnly:
			score = 2
		}
		if score > bestScore || (score == bestScore && f.Bitrate > best.Bitrate) {
			best, bestScore, found = f, score, true
		}
	}
	return best, found
}

// itHasAudioCodec reports whether a muxed mimeType carries an audio track.
// itVideoFromMusicItem reads the YouTube Music search row shape, whose fields
// are split across flex columns and whose duration arrives as words rather than
// digits.
func itVideoFromMusicItem(m map[string]any) (ytMedia, bool) {
	id := ""
	if data, ok := m["playlistItemData"].(map[string]any); ok {
		if s, ok := data["videoId"].(string); ok {
			id = s
		}
	}
	if !youtubeID.MatchString(id) {
		id = itFlexColumnVideoID(m["flexColumns"])
	}
	if !youtubeID.MatchString(id) {
		return ytMedia{}, false
	}
	columns := itFlexColumns(m["flexColumns"])
	if len(columns) == 0 {
		return ytMedia{}, false
	}
	title := columns[0]
	author := ""
	if len(columns) > 1 {
		author = itMusicByline(columns[1])
	}
	seconds, ok := 0.0, false
	for _, column := range columns[1:] {
		if s, found := itMusicDuration(column); found {
			seconds, ok = s, true
			break
		}
	}
	if !ok {
		// Duration may also arrive as a plain label in a fixed column.
		seconds, ok = itFixedColumnDuration(m["fixedColumns"])
	}
	if !ok {
		return ytMedia{}, false // an album, artist or playlist row
	}
	thumb := ""
	if th, ok := m["thumbnail"].(map[string]any); ok {
		if mt, ok := th["musicThumbnailRenderer"].(map[string]any); ok {
			thumb = itSearchThumbnail(mt["thumbnail"])
		}
	}
	if thumb == "" {
		thumb = "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
	}
	return ytMedia{
		ID:         id,
		Title:      title,
		Uploader:   author,
		Duration:   seconds,
		Thumbnail:  thumb,
		WebpageURL: "https://www.youtube.com/watch?v=" + id,
	}, true
}

// itFlexColumns joins the runs of each flex column into one string.
func itFlexColumns(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		em, ok := entry.(map[string]any)
		if !ok {
			out = append(out, "")
			continue
		}
		col, _ := em["musicResponsiveListItemFlexColumnRenderer"].(map[string]any)
		out = append(out, itSearchText(col["text"]))
	}
	return out
}

// itFlexColumnVideoID reads the video id from a row's first column, which links
// to the song it names.
func itFlexColumnVideoID(v any) string {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	return itWatchEndpointVideoID(list[0])
}

// itMusicByline keeps the artist from a byline that also lists the album, the
// year and the duration, separated by bullets.
func itMusicByline(s string) string {
	if i := strings.Index(s, " • "); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// itMusicDuration finds the duration in a music row's byline column, which reads
// "Artist • Album • 2:27". The word form ("2 minutes, 27 seconds") appears in
// the accessibility label, so both are accepted.
func itMusicDuration(column string) (float64, bool) {
	parts := strings.Split(column, " • ")
	for i := len(parts) - 1; i >= 0; i-- {
		if seconds, ok := itLengthSeconds(parts[i]); ok {
			return seconds, true
		}
	}
	return itDurationWords(column)
}

// itDurationWords parses the words YouTube Music uses, such as
// "2 minutes, 27 seconds" or "1 hour, 32 minutes, 44 seconds".
func itDurationWords(s string) (float64, bool) {
	matches := itDurationWord.FindAllStringSubmatch(strings.ToLower(s), -1)
	if len(matches) == 0 {
		return 0, false
	}
	total, unit := 0.0, 0.0
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(m[2], "hour"):
			unit = 3600
		case strings.HasPrefix(m[2], "minute"):
			unit = 60
		default:
			unit = 1
		}
		total += float64(n) * unit
	}
	return total, total > 0
}

// itFixedColumnDuration reads a "3:45" label from the fixed columns.
func itFixedColumnDuration(v any) (float64, bool) {
	list, ok := v.([]any)
	if !ok {
		return 0, false
	}
	for _, entry := range list {
		em, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		col, _ := em["musicResponsiveListItemFixedColumnRenderer"].(map[string]any)
		if seconds, ok := itLengthSeconds(itSearchText(col["text"])); ok {
			return seconds, true
		}
	}
	return 0, false
}

func itHasAudioCodec(mime string) bool {
	for _, codec := range []string{"mp4a", "opus", "vorbis", "ec-3", "ac-3", "flac"} {
		if strings.Contains(mime, codec) {
			return true
		}
	}
	return false
}

// itPlayerResolve asks each client in turn until one returns playable formats.
func (s *Service) itPlayerResolve(ctx context.Context, videoID string) (itPlayer, itFormat, error) {
	var last error
	for _, c := range s.itClients() {
		version := c.Version
		body := map[string]any{
			"context":        s.itContext(c, version),
			"videoId":        videoID,
			"contentCheckOk": true,
			"racyCheckOk":    true,
		}
		b, err := s.itCall(ctx, c, version, "player", body)
		if err != nil {
			last = fmt.Errorf("%s: %w", c.Name, err)
			continue
		}
		var p itPlayer
		if err = json.Unmarshal(b, &p); err != nil {
			last = fmt.Errorf("%s: %w", c.Name, err)
			continue
		}
		if p.PlayabilityStatus.Status != "OK" {
			last = fmt.Errorf("%s: youtube reported %s %s", c.Name, p.PlayabilityStatus.Status, strings.TrimSpace(p.PlayabilityStatus.Reason))
			continue
		}
		f, ok := pickFormat(p)
		if !ok {
			last = fmt.Errorf("%s: no playable audio format for %s", c.Name, videoID)
			continue
		}
		return p, f, nil
	}
	if last == nil {
		last = errors.New("no InnerTube client configured")
	}
	return itPlayer{}, itFormat{}, last
}

// itMediaURL resolves a direct stream URL for a video id.
func (s *Service) itMediaURL(ctx context.Context, videoID string) (string, map[string]string, error) {
	_, f, err := s.itPlayerResolve(ctx, videoID)
	if err != nil {
		return "", nil, err
	}
	return f.URL, nil, nil
}

func itPlayerMedia(p itPlayer) ytMedia {
	thumb := "https://i.ytimg.com/vi/" + p.VideoDetails.VideoID + "/hqdefault.jpg"
	if n := len(p.VideoDetails.Thumbnail.Thumbnails); n > 0 {
		thumb = p.VideoDetails.Thumbnail.Thumbnails[n-1].URL
	}
	seconds, _ := strconv.ParseFloat(p.VideoDetails.LengthSeconds, 64)
	return ytMedia{
		ID:         p.VideoDetails.VideoID,
		Title:      p.VideoDetails.Title,
		Uploader:   p.VideoDetails.Author,
		Duration:   seconds,
		IsLive:     p.VideoDetails.IsLiveContent,
		Thumbnail:  thumb,
		WebpageURL: "https://www.youtube.com/watch?v=" + p.VideoDetails.VideoID,
	}
}

// itSearchText reads the several shapes YouTube uses for a text field.
func itSearchText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["simpleText"].(string); ok {
			return s
		}
		if s, ok := t["content"].(string); ok {
			return s
		}
		if runs, ok := t["runs"].([]any); ok {
			var b strings.Builder
			for _, r := range runs {
				if m, ok := r.(map[string]any); ok {
					if s, ok := m["text"].(string); ok {
						b.WriteString(s)
					}
				}
			}
			return b.String()
		}
	}
	return ""
}

func itSearchThumbnail(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	list, ok := m["thumbnails"].([]any)
	if !ok {
		return ""
	}
	best := ""
	for _, e := range list {
		if em, ok := e.(map[string]any); ok {
			if s, ok := em["url"].(string); ok {
				best = s
			}
		}
	}
	return best
}

// itLengthSeconds parses the "3:45" and "1:02:03" labels. An item without one
// is a live stream, a channel or a playlist, none of which is a loadable track.
func itLengthSeconds(s string) (float64, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0.0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + float64(n)
	}
	return total, true
}

func itVideoFromRenderer(m map[string]any) (ytMedia, bool) {
	id, _ := m["videoId"].(string)
	if !youtubeID.MatchString(id) {
		return ytMedia{}, false
	}
	length, ok := itLengthSeconds(itSearchText(m["lengthText"]))
	if !ok {
		return ytMedia{}, false
	}
	author := itSearchText(m["ownerText"])
	if author == "" {
		author = itSearchText(m["longBylineText"])
	}
	if author == "" {
		author = itSearchText(m["shortBylineText"])
	}
	return ytMedia{ID: id, Title: itSearchText(m["title"]), Uploader: author, Duration: length, Thumbnail: itSearchThumbnail(m["thumbnail"]), WebpageURL: "https://www.youtube.com/watch?v=" + id}, true
}

// itVideoFromLockup reads the newer lockupViewModel shape, which carries its
// fields inside nested metadata view models instead of renderer fields.
func itVideoFromLockup(m map[string]any) (ytMedia, bool) {
	id, _ := m["contentId"].(string)
	if !youtubeID.MatchString(id) {
		return ytMedia{}, false
	}
	var title, author, length string
	var thumb string
	if md, ok := m["metadata"].(map[string]any); ok {
		if lm, ok := md["lockupMetadataViewModel"].(map[string]any); ok {
			title = itSearchText(lm["title"])
			if meta, ok := lm["metadata"].(map[string]any); ok {
				if cm, ok := meta["contentMetadataViewModel"].(map[string]any); ok {
					parts := itMetadataParts(cm["metadataRows"])
					if len(parts) > 0 {
						author = parts[0]
					}
					for _, p := range parts[1:] {
						if _, ok := itLengthSeconds(p); ok {
							length = p
						}
					}
				}
			}
		}
	}
	if ci, ok := m["contentImage"].(map[string]any); ok {
		if tv, ok := ci["thumbnailViewModel"].(map[string]any); ok {
			thumb = itSearchThumbnail(tv["image"])
		}
	}
	seconds, ok := itLengthSeconds(length)
	if !ok {
		return ytMedia{}, false
	}
	return ytMedia{ID: id, Title: title, Uploader: author, Duration: seconds, Thumbnail: thumb, WebpageURL: "https://www.youtube.com/watch?v=" + id}, true
}

func itMetadataParts(v any) []string {
	rows, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, row := range rows {
		rm, ok := row.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := rm["metadataParts"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if text := itSearchText(pm["text"]); text != "" {
					out = append(out, text)
				}
			}
		}
	}
	return out
}

// itVideoFromVideoData reads the videoData shape, which YouTube serves inside
// elementRenderer/videoWithContextModel entries. It appears in playlist browse
// responses and shares its metadata layout with the lockup view model.
func itVideoFromVideoData(m map[string]any) (ytMedia, bool) {
	id := ""
	if s, ok := m["dragAndDropUrl"].(string); ok {
		id = itVideoID(s)
	}
	if !youtubeID.MatchString(id) {
		id = itWatchEndpointVideoID(m["onTap"])
	}
	if !youtubeID.MatchString(id) {
		return ytMedia{}, false
	}
	lockup, _ := m["lockupMetadata"].(map[string]any)
	vm, _ := lockup["lockupMetadataViewModel"].(map[string]any)
	title := itSearchText(vm["title"])
	author, length := "", ""
	if md, ok := vm["metadata"].(map[string]any); ok {
		if cm, ok := md["contentMetadataViewModel"].(map[string]any); ok {
			parts := itMetadataParts(cm["metadataRows"])
			if len(parts) > 0 {
				author = parts[0]
			}
			for _, p := range parts[1:] {
				if _, ok := itLengthSeconds(p); ok {
					length = p
				}
			}
		}
	}
	var thumb string
	if th, ok := m["thumbnail"].(map[string]any); ok {
		if s, ok := th["timestampText"].(string); ok && length == "" {
			length = s
		}
		thumb = itImageSources(th["image"])
	}
	seconds, ok := itLengthSeconds(length)
	if !ok {
		return ytMedia{}, false // lives and premieres carry no timestamp
	}
	if thumb == "" {
		thumb = "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
	}
	return ytMedia{
		ID:         id,
		Title:      title,
		Uploader:   author,
		Duration:   seconds,
		Thumbnail:  thumb,
		WebpageURL: "https://www.youtube.com/watch?v=" + id,
	}, true
}

// itWatchEndpointVideoID digs a video id out of an onTap command.
func itWatchEndpointVideoID(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if cmd, ok := m["innertubeCommand"].(map[string]any); ok {
		if we, ok := cmd["watchEndpoint"].(map[string]any); ok {
			if id, ok := we["videoId"].(string); ok && youtubeID.MatchString(id) {
				return id
			}
		}
	}
	return ""
}

// itImageSources reads the image.sources shape, preferring the largest entry.
func itImageSources(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	list, ok := m["sources"].([]any)
	if !ok {
		return ""
	}
	best, bestWidth := "", -1
	for _, e := range list {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		url, ok := em["url"].(string)
		if !ok {
			continue
		}
		width := 0
		if w, ok := em["width"].(float64); ok {
			width = int(w)
		}
		if width >= bestWidth {
			best, bestWidth = url, width
		}
	}
	return best
}

// itCollectVideos walks a response and collects every loadable video, in the
// order YouTube returned them. seen dedupes across shapes, because one response
// may describe the same video both as a renderer and as an element.
func itCollectVideos(v any, out *[]ytMedia, seen map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			cm, isMap := child.(map[string]any)
			if isMap {
				var (
					video ytMedia
					ok    bool
				)
				switch k {
				case "videoRenderer", "compactVideoRenderer", "playlistVideoRenderer":
					video, ok = itVideoFromRenderer(cm)
				case "lockupViewModel":
					video, ok = itVideoFromLockup(cm)
				case "videoData":
					video, ok = itVideoFromVideoData(cm)
				case "musicResponsiveListItemRenderer":
					video, ok = itVideoFromMusicItem(cm)
				}
				if ok && !seen[video.ID] {
					seen[video.ID] = true
					*out = append(*out, video)
				}
			}
			itCollectVideos(child, out, seen)
		}
	case []any:
		for _, child := range t {
			itCollectVideos(child, out, seen)
		}
	}
}

// ytSearchBrackets matches the bracketed groups titles carry around artist
// credits, which YouTube matches literally and which therefore often make a rare
// title unfindable.
var ytSearchBrackets = regexp.MustCompile(`（[^（）]*）|\([^()]*\)|\[[^\[\]]*\]|【[^【】]*】|「[^「」]*」|『[^『』]*』`)

// ytSearchSeparators matches the separators used between a title and an artist.
var ytSearchSeparators = regexp.MustCompile(`\s+[-–—|/／]\s+`)

// ytSearchHints matches the request hints people append to a search, which are
// never part of a title.
var ytSearchHints = regexp.MustCompile(`(?i)(^|\s)(audio|lyrics?|official(\s+(video|audio|music\s+video))?|mv|pv|full\s*ver\.?|hd|hq|4k|cover)(\s|$)`)

// ytSearchMaxVariants caps how many simplified queries one lookup may try, so a
// query that genuinely has no results cannot turn into a request storm.
const ytSearchMaxVariants = 3

// itSearchVariants returns the queries to try in order: the query itself first,
// then progressively simplified forms.
//
// YouTube matches rare titles strictly: measured on 2026-10-08, the query
// "椎葉 紬（CV：黒咲そら） - Sweet Sweet アリス audio" returned
// searchNoResultsRenderer on ANDROID, ANDROID_VR and IOS (and no results through
// yt-dlp either), while either half on its own returned 20 videos with the same
// clients and the same settings. Dropping the bracket, the separator and the
// hint therefore turns an unplayable request into a playable one. No variant is
// tried unless the previous one came back empty, so ordinary queries still cost
// exactly one request.
func itSearchVariants(query string) []string {
	out := []string{query}
	seen := map[string]bool{query: true}
	add := func(q string) {
		q = strings.Join(strings.Fields(q), " ")
		if q == "" || seen[q] || len(out) > ytSearchMaxVariants {
			return
		}
		seen[q] = true
		out = append(out, q)
	}
	cleaned := cleanYTQuery(query)
	add(cleaned)
	// The two halves of "artist - title" are each far more likely to match than
	// the pair, so try the longer half first.
	sides := ytSearchSeparators.Split(query, -1)
	if len(sides) > 1 {
		sort.SliceStable(sides, func(i, j int) bool { return len([]rune(sides[i])) > len([]rune(sides[j])) })
		for _, side := range sides {
			add(cleanYTQuery(side))
		}
	}
	// A leading run of the query, for titles that are simply too long.
	if fields := strings.Fields(cleaned); len(fields) > 8 {
		add(strings.Join(fields[:8], " "))
	}
	return out
}

// cleanYTQuery removes the decoration YouTube would otherwise match literally.
func cleanYTQuery(q string) string {
	q = ytSearchBrackets.ReplaceAllString(q, " ")
	q = ytSearchSeparators.ReplaceAllString(q, " ")
	q = ytSearchHints.ReplaceAllString(q, " ")
	return strings.Join(strings.Fields(q), " ")
}

// itSearch returns playable videos for a query, in YouTube's own order. An empty
// result from YouTube is retried with the simplified queries above, because an
// empty answer is usually about the query text rather than about the client.
func (s *Service) itSearch(ctx context.Context, query string, want int) ([]ytMedia, error) {
	var last error
	for i, q := range itSearchVariants(query) {
		videos, err := s.itSearchQuery(ctx, q, want)
		if err != nil {
			last = err
			continue
		}
		if i > 0 {
			s.log().Debug("youtube search needed a simplified query",
				"original", query,
				"used", q,
				"attempt", i+1,
			)
		}
		return videos, nil
	}
	if last == nil {
		last = errors.New("no InnerTube client configured")
	}
	return nil, last
}

// itSearchQuery runs one query against every configured client.
func (s *Service) itSearchQuery(ctx context.Context, query string, want int) ([]ytMedia, error) {
	var last error
	for _, c := range s.itClients() {
		version := c.Version
		body := map[string]any{
			"context": s.itContext(c, version),
			"query":   query,
			"params":  itSearchParams,
		}
		b, err := s.itCall(ctx, c, version, "search", body)
		if err != nil {
			last = fmt.Errorf("%s: %w", c.Name, err)
			continue
		}
		var raw any
		if err = json.Unmarshal(b, &raw); err != nil {
			last = fmt.Errorf("%s: %w", c.Name, err)
			continue
		}
		var videos []ytMedia
		itCollectVideos(raw, &videos, map[string]bool{})
		if len(videos) == 0 {
			last = fmt.Errorf("%s: search returned no videos", c.Name)
			continue
		}
		if want > 0 && len(videos) > want {
			videos = videos[:want]
		}
		return videos, nil
	}
	if last == nil {
		last = errors.New("no InnerTube client configured")
	}
	return nil, last
}

// itMusicSearch returns songs from YouTube Music. It is a separate host and
// client from the video search, and results are marked with the music row shape.
// It shares the simplified-query retry, because the same strict matching applies.
func (s *Service) itMusicSearch(ctx context.Context, query string, want int) ([]ytMedia, error) {
	var last error
	for i, q := range itSearchVariants(query) {
		videos, err := s.itMusicSearchQuery(ctx, q, want)
		if err != nil {
			last = err
			continue
		}
		if i > 0 {
			s.log().Debug("youtube music search needed a simplified query",
				"original", query,
				"used", q,
				"attempt", i+1,
			)
		}
		return videos, nil
	}
	if last == nil {
		last = errors.New("no InnerTube client configured")
	}
	return nil, last
}

func (s *Service) itMusicSearchQuery(ctx context.Context, query string, want int) ([]ytMedia, error) {
	version := itMusicClient.Version
	if v := strings.TrimSpace(s.Config.YoutubeClientVersion); v != "" {
		version = v
	}
	body := map[string]any{
		"context": s.itContext(itMusicClient, version),
		"query":   query,
		"params":  itMusicSongsParams,
	}
	b, err := s.itCallHost(ctx, s.Config.YoutubeMusicEndpoint, itMusicClient, version, "search", body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", itMusicClient.Name, err)
	}
	var raw any
	if err = json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", itMusicClient.Name, err)
	}
	var videos []ytMedia
	itCollectVideos(raw, &videos, map[string]bool{})
	if len(videos) == 0 {
		return nil, fmt.Errorf("%s: search returned no songs", itMusicClient.Name)
	}
	if want > 0 && len(videos) > want {
		videos = videos[:want]
	}
	return videos, nil
}

// itVideoID reads a video id out of a watch URL or bare id.
func itVideoID(id string) string {
	if youtubeID.MatchString(id) {
		return id
	}
	if !strings.HasPrefix(id, "http") {
		return ""
	}
	u, err := url.Parse(id)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "youtu.be" {
		return ""
	}
	if v := u.Query().Get("v"); youtubeID.MatchString(v) {
		return v
	}
	if host == "youtu.be" {
		if v := strings.Trim(u.Path, "/"); youtubeID.MatchString(v) {
			return v
		}
	}
	return ""
}

// itMaxPlaylistPages bounds how far a playlist is paged. Upstream stops after a
// fixed number of pages too, so a huge playlist cannot stall a load request.
const itMaxPlaylistPages = 6

// itPlaylistID reads a playlist id out of a playlist URL or a bare playlist id.
// A watch URL carrying list= counts as a playlist, matching what yt-dlp does
// when it is not told to ignore playlists.
func itPlaylistID(id string) string {
	if strings.HasPrefix(id, "http") {
		u, err := url.Parse(id)
		if err != nil {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "youtu.be" {
			return ""
		}
		if v := u.Query().Get("list"); itPlaylistBody(v) {
			return v
		}
		return ""
	}
	if v, ok := strings.CutPrefix(id, "VL"); ok && itPlaylistBody(v) {
		return v
	}
	if itPlaylistBody(id) {
		return id
	}
	return ""
}

// itPlaylistBody reports whether s has the shape of a playlist id. Video ids are
// 11 characters, so requiring at least 12 keeps the two apart.
func itPlaylistBody(s string) bool {
	if len(s) < 12 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// itPlaylistPage splits one browse payload into videos, the token for the next
// page, and the playlist title.
func itPlaylistPage(raw any) (videos []ytMedia, continuation, title string) {
	itCollectVideos(raw, &videos, map[string]bool{})
	itPlaylistMeta(raw, &continuation, &title)
	return videos, continuation, title
}

// itPlaylistMeta collects the next-page token and the playlist title. YouTube
// uses two continuation shapes depending on the playlist and the rollout:
// continuations[].nextContinuationData.continuation (older) and
// continuationItemRenderer.continuationEndpoint.continuationCommand.token
// (newer). Both are searched for wherever they appear, because their nesting
// moves between responses.
func itPlaylistMeta(v any, continuation, title *string) {
	switch t := v.(type) {
	case map[string]any:
		if *continuation == "" {
			if ncd, ok := t["nextContinuationData"].(map[string]any); ok {
				if token, ok := ncd["continuation"].(string); ok && token != "" {
					*continuation = token
				}
			}
		}
		if *continuation == "" {
			if cc, ok := t["continuationCommand"].(map[string]any); ok {
				if token, ok := cc["token"].(string); ok && token != "" {
					*continuation = token
				}
			}
		}
		if *title == "" {
			if h, ok := t["playlistHeaderRenderer"].(map[string]any); ok {
				*title = itSearchText(h["title"])
			}
			if h, ok := t["pageHeaderViewModel"].(map[string]any); ok && *title == "" {
				*title = itPageHeaderTitle(h["title"])
			}
		}
		for _, child := range t {
			itPlaylistMeta(child, continuation, title)
		}
	case []any:
		for _, child := range t {
			itPlaylistMeta(child, continuation, title)
		}
	}
}

// itPageHeaderTitle reads the newer header shape, whose text lives in a nested
// view model rather than in runs.
func itPageHeaderTitle(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if dt, ok := m["dynamicTextViewModel"].(map[string]any); ok {
		if text, ok := dt["text"].(map[string]any); ok {
			if s, ok := text["content"].(string); ok {
				return s
			}
		}
	}
	return itSearchText(m)
}

// itPlaylist resolves a playlist. Paging stops when the playlist ends, when the
// page limit is reached, or when maxTracks entries are collected; a failure on a
// later page keeps the entries already gathered instead of discarding them.
func (s *Service) itPlaylist(ctx context.Context, playlistID string, maxTracks int) (itPlaylistResult, error) {
	var last error
	for _, c := range s.itClients() {
		version := c.Version
		body := map[string]any{"context": s.itContext(c, version), "browseId": "VL" + playlistID}
		b, err := s.itCall(ctx, c, version, "browse", body)
		if err != nil {
			last = fmt.Errorf("%s: %w", c.Name, err)
			continue
		}
		var raw any
		if err = json.Unmarshal(b, &raw); err != nil {
			last = fmt.Errorf("%s: %w", c.Name, err)
			continue
		}
		videos, continuation, title := itPlaylistPage(raw)
		if len(videos) == 0 {
			last = fmt.Errorf("%s: playlist %s returned no videos", c.Name, playlistID)
			continue
		}
		out := itDedupeVideos(videos)
		for pages := 1; continuation != "" && pages < itMaxPlaylistPages; pages++ {
			if maxTracks > 0 && len(out) >= maxTracks {
				break
			}
			next, err := s.itCall(ctx, c, version, "browse", map[string]any{"context": s.itContext(c, version), "continuation": continuation})
			if err != nil {
				break // keep the page we already have
			}
			var rawNext any
			if err = json.Unmarshal(next, &rawNext); err != nil {
				break
			}
			more, token, _ := itPlaylistPage(rawNext)
			if len(more) == 0 {
				break
			}
			continuation = token
			out = itDedupeVideos(append(out, more...))
		}
		res := itPlaylistResult{Title: title, Videos: out}
		if maxTracks > 0 && len(out) > maxTracks {
			res.Videos = out[:maxTracks]
			res.Truncated = true
		} else if maxTracks > 0 && len(out) == maxTracks && continuation != "" {
			res.Truncated = true
		}
		return res, nil
	}
	if last == nil {
		last = errors.New("no InnerTube client configured")
	}
	return itPlaylistResult{}, last
}

type itPlaylistResult struct {
	Title     string
	Videos    []ytMedia
	Truncated bool
}

func itDedupeVideos(in []ytMedia) []ytMedia {
	out := make([]ytMedia, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// loadYouTubeNative serves single videos, searches and playlists without yt-dlp.
func (s *Service) loadYouTubeNative(ctx context.Context, id string) (protocol.LoadResult, error) {
	if strings.HasPrefix(id, "ytsearch:") || strings.HasPrefix(id, "ytmsearch:") {
		query := id[strings.Index(id, ":")+1:]
		if strings.TrimSpace(query) == "" {
			return protocol.LoadResult{}, errors.New("search query is empty")
		}
		var tracks []ytMedia
		var err error
		if strings.HasPrefix(id, "ytmsearch:") {
			// YouTube Music is the source LavaSrc uses for ytmsearch:. If it
			// comes back empty, the plain video search is still a usable answer.
			if tracks, err = s.itMusicSearch(ctx, query, s.Config.SearchResults); err != nil {
				tracks, err = s.itSearch(ctx, query, s.Config.SearchResults)
			}
		} else {
			tracks, err = s.itSearch(ctx, query, s.Config.SearchResults)
		}
		if err != nil {
			return protocol.LoadResult{}, err
		}
		out := make([]*protocol.Track, 0, len(tracks))
		for _, m := range tracks {
			t, err := mediaTrack(m, "youtube")
			if err != nil {
				return protocol.LoadResult{}, err
			}
			out = append(out, t)
		}
		if len(out) == 0 {
			return protocol.LoadResult{LoadType: "empty", Data: nil}, nil
		}
		return protocol.LoadResult{LoadType: "search", Data: out}, nil
	}
	if playlistID := itPlaylistID(id); playlistID != "" {
		return s.loadYouTubePlaylist(ctx, playlistID)
	}
	videoID := itVideoID(id)
	if videoID == "" {
		return protocol.LoadResult{}, fmt.Errorf("cannot read a video id from %q", id)
	}
	p, _, err := s.itPlayerResolve(ctx, videoID)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	t, err := mediaTrack(itPlayerMedia(p), "youtube")
	if err != nil {
		return protocol.LoadResult{}, err
	}
	return protocol.LoadResult{LoadType: "track", Data: t}, nil
}

func (s *Service) loadYouTubePlaylist(ctx context.Context, playlistID string) (protocol.LoadResult, error) {
	pl, err := s.itPlaylist(ctx, playlistID, s.Config.MaxPlaylistTracks)
	if err != nil {
		return protocol.LoadResult{}, err
	}
	if len(pl.Videos) == 0 {
		return protocol.LoadResult{LoadType: "empty", Data: nil}, nil
	}
	out := make([]*protocol.Track, 0, len(pl.Videos))
	for _, m := range pl.Videos {
		t, err := mediaTrack(m, "youtube")
		if err != nil {
			return protocol.LoadResult{}, err
		}
		out = append(out, t)
	}
	name := pl.Title
	if name == "" {
		name = playlistID
	}
	plugin := protocol.Object{}
	if pl.Truncated {
		plugin["truncated"] = true
	}
	return protocol.LoadResult{LoadType: "playlist", Data: protocol.Playlist{
		Info:       protocol.PlaylistInfo{Name: name, SelectedTrack: -1},
		PluginInfo: plugin,
		Tracks:     out,
	}}, nil
}
