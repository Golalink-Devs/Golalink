# Compatibility

Golalink targets the Lavalink 4.2 API. This page describes what that means in
practice: what is implemented, where behaviour deliberately differs, and what is
missing. It is not a claim of byte-for-byte equality with Lavalink.

The differences marked "deliberate" are choices, not gaps. They are listed so a
client author is not surprised by them.

## Scope

| Area | Status | Behaviour |
|---|---|---|
| `/version`, `/v4/info`, `/v4/stats` | Implemented | `/version` reports the protocol version; the Golalink version appears under `info.golalink` |
| `loadtracks`, `decodetrack`, `decodetracks` | Implemented | v4 load types preserved; a batch decodes up to 1000 tracks |
| Track encoding | Implemented | Java modified UTF-8, v1 to v3 decoding, v3 encoding; the input encoding is preserved |
| Track objects across nodes | Partial | Standard fields and source-specific extension bytes are read, but newly encoded HTTP and Spotify tracks are not guaranteed to play on another implementation |
| Session resuming | Implemented | Requires the same bot ID, a disconnected session that has not timed out, and is held in memory only; a restart drops it |
| Player REST | Implemented | `volume`, `pause`, `position`, `endTime`, `filters`, `voice`, `track`, `noReplace`, `userData` |
| Deprecated track fields | Implemented | `encodedTrack` and `identifier` are accepted and are mutually exclusive with `track` |
| Events | Implemented | `ready`, `stats`, `playerUpdate`, start, end, exception, stuck, voice closed; Golalink also sends `BotConnectEvent` and `BotDisconnectEvent` when the bot joins or leaves a voice channel |
| Discord voice | Implemented | DisGo UDP transport and DAVE; v4.2 `channelId` is required |
| Filters | Implemented | All ten standard fields, applied in process |
| Audio formats | Implemented | MP3, WAV, FLAC at any bit depth, Ogg Vorbis, Ogg Opus, Ogg FLAC, AAC in ADTS or MP4, and Opus in WebM, decoded in process. FFmpeg is the fall back for HE-AAC, multichannel AAC, and Vorbis or AAC inside WebM |
| Surround | Implemented | Up to eight channels, folded to stereo |
| Lyrics | Implemented | The LavaLyrics routes, answered from LRCLIB and, for what it has nothing for, from YouTube Music |
| Prometheus metrics | Implemented | `golalink_` metric names, not a copy of any other implementation's metric set |
| IP route planner | Not implemented | `GET /v4/routeplanner/status` returns 204, free operations return 501 |
| Java plugins | Not compatible | A JVM plugin cannot run inside a Go process |
| Cluster and high availability | Not implemented | One process holds one in-memory state; run several nodes and let the client balance |

A filter or parameter that is not accepted returns `400` rather than being
silently ignored. If a load fails after a player was created, the empty player is
kept so a concurrent `PATCH` on the same object is not pulled out from under the
client; delete it explicitly.

## Aligned with Lavalink 4.2.2

| Item | Behaviour |
|---|---|
| `PATCH .../players/{guildId}` | `200` with the player body, not `204` |
| `POST /v4/decodetracks` | Request and response are both a plain JSON array |
| `PATCH /v4/sessions/{id}` | `200`; `timeout` is in seconds |
| `DELETE .../players/{guildId}` | `204`, idempotent |
| `GET /v4/routeplanner/status` while disabled | `204` with no body |
| `empty` load type | `data` is `null` |
| `pluginInfo` and `userData` | Always present, even when empty |
| Requesting a resumable session over REST | `404 session not found`, while the session keeps its state internally |
| `equalizer` band `gain` when absent | Defaults to `1.0` |
| `endTime` reached | Reported as `finished` |
| Track end reasons | `finished`, `loadFailed`, `stopped`, `replaced`, `cleanup` |
| WebSocket handshake | Sends `Session-Resumed` |
| Response headers | Every response carries `Lavalink-Api-Version: 4` |
| Client WebSocket messages | Ignored, as in v4; do not use `op: 1` or `op: 2` as a keep-alive |
| Numeric fields sent as strings | Accepted. Lavalink is Jackson-based and coerces `"position": "0"` into an `int`; Go's decoder rejects it, so `position`, `endTime` and `volume` are parsed from either form |
| `/v4/encodetrack` | Does not exist, in either implementation; encode through `loadtracks` or `decodetrack` |
| `/v4/sessions` (list) | Does not exist |

## Deliberate differences

| Item | Lavalink | Golalink | Why |
|---|---|---|---|
| `/version` authentication | Requires the password | Unauthenticated | It reports a protocol version, not state, and a probe should not need a secret |
| `/healthz` | Does not exist | Added, unauthenticated | Container and load-balancer health checks |
| `volume` out of range | Silently clamped to 0–1000, `200` | `400 volume must be 0..1000` | A client sending an impossible value should find out |
| `/v4/stats` `frameStats` | Present but `null` in practice | Key omitted from the REST body | The WebSocket `stats` event still carries a real `frameStats` object with `sent`, `nulled` and `deficit` |
| `POST /v4/decodetracks` partial failure | `500` for the whole batch | `400` with the decoding message | More specific |
| Route planner free operations while disabled | `500` | `501` | Not implemented is not an internal error |
| `/metrics` authentication | Unauthenticated once enabled | Requires the node password | Metrics leak session and player counts |
| Unknown filter keys | Accepted through a plugin filter registry | `400 unsupported filter: <name>` | There is no plugin system, and accepting a filter that does nothing misleads the client |
| Filter validation order | Disabled filters are checked first | Values are validated first, then disabled filters | One validation entry point; both cases return `400`, only the message differs for a request that is wrong in both ways |
| Unknown configuration fields | Usually ignored | Startup fails and names the field | A misspelled option is harder to find than a startup error |

## Response shape

Filters are returned exactly as the client sent them. Lavalink re-encodes filters
through a typed model where every field is marked for default encoding, so its
responses add fields the client omitted: sending `{"timescale":{"speed":1.5}}`
comes back as `{"timescale":{"speed":1.5,"pitch":1.0,"rate":1.0}}`, along with
every `karaoke` field.

Golalink does not add those defaults. A client that decodes into a typed model
applies the same defaults itself and sees no difference; only a literal
field-by-field comparison of the raw JSON would. Returning what was sent loses no
information and keeps the response smaller.

## Operational differences

| Item | Lavalink | Golalink |
|---|---|---|
| Configuration | `application.yml` | `config.yaml`, and `config.json` still loads |
| Log levels and format | `logback.xml` | `logLevel`, `logFormat`, `logColor`, `logDirectory` in the same file |
| Failed requests | Whatever Logback is configured for | Always logged: 4xx at `warn`, 5xx at `error`, with method, path, status, size, source and duration |
| `/v4/info` `sourceManagers` | The loaded source managers | The sources that actually work, including non-standard names `yt-dlp` and `deezer` |

## Upstream limits

These are properties of the sources, not of the protocol layer.

**YouTube.** The native InnerTube client resolves search, single videos and
playlists, and resolves the short-lived audio URL only at playback time. YouTube
currently serves only the muxed progressive format without a proof-of-origin
token, which is roughly 128 kbps AAC; audio-only adaptive formats need SABR or a
token, and neither is implemented.

**YouTube Music.** `ytmsearch:` uses the YouTube Music WEB_REMIX client with a
songs filter, and falls back to an ordinary YouTube search if that source fails.

**SoundCloud.** A native client against the public api-v2 reads search, single
tracks and sets, and picks a progressive MP3 that FFmpeg can read without HLS.
The client id is scraped from the site's JavaScript bundle and cached; the page
sometimes serves a stub without a bundle, in which case `/discover` is tried, and
if both fail the load falls back to yt-dlp and the playback error names both
reasons.

**Spotify.** Spotify is used for metadata only. A track is matched to a YouTube
source by title, artist and duration, and playback is that YouTube audio; Spotify
audio is never downloaded or decrypted. Playlists are read from `/items` first and
`/tracks` second so that both new and old applications work. Reading your own or a
collaborative playlist needs a user authorisation refresh token; client
credentials alone are not enough for those, and algorithmically generated
playlists (`37i9dQZ...`) are refused with a clear error instead of a request that
cannot succeed. Artist top tracks come from `/artists/{id}/top-tracks`.

**Deezer.** Deezer is also metadata only, read from the public API without
credentials: tracks, albums and playlists, including both playlist pagination
shapes. Like Spotify, `dzsearch:` and `dzisrc:` need a Deezer account and are
refused with an explanation rather than returning nothing. `link.deezer.com`
short links are not expanded; use a full `deezer.com` URL.

**HTTP and containers.** A direct HTTP(S) source is read through Go's HTTP client
with a check on every dial and redirect, and FFmpeg only ever reads from a pipe,
so it cannot be pointed at a local file or a URL. Seeking within a plain HTTP
stream re-decodes from the start, which can be slow on a long file. HLS and DASH
manifests, DRM and member-only content are not supported: a direct audio stream is
required.

## Not planned

SABR streaming for YouTube audio-only formats, IP route planning, a Java plugin
host, recording, multi-worker control, and the NodeLink-specific extensions are
all out of scope for the current design.
