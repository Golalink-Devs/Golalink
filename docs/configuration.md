# Configuration

Golalink reads one YAML file. When `-config` is not given it looks for
`config.yaml`, then `config.yml`, then `config.json`, in the working directory.
JSON is accepted because it is a subset of YAML.

Only the options you want to change need to be present; everything else falls
back to the default. Field names are matched strictly, so an unknown or
misspelled key stops startup with the offending name instead of being ignored.
Secrets are normally supplied through environment variables, which always
override the file.

`config.example.yaml` is the complete list with a one-line comment per option.

## Command line

| Flag | Effect |
|---|---|
| `-config PATH` | Read this file instead of searching the working directory |
| `-check` | Validate the configuration and the media tools, then exit |
| `-version` | Print the version and exit |

## Node

| Option | Default | Meaning |
|---|---|---|
| `address` | `127.0.0.1:2333` | Address the HTTP and WebSocket server binds to |
| `password` | none | Node password; startup fails while it is empty |

`/healthz` and `/version` are unauthenticated so a supervisor can probe the
process. Every other endpoint, including the WebSocket and `/metrics`, requires
the node password.

## Media tools

Golalink decodes, filters and Opus-encodes in process where it can, and falls
back to FFmpeg for the rest; yt-dlp is an optional fallback resolver. FFmpeg and
yt-dlp are taken from `toolsDirectory`; the system copies are never used, so one
node always runs the same builds. Container metadata is read in process, so no
probe binary is needed.

| Option | Default | Meaning |
|---|---|---|
| `ffmpeg` | `ffmpeg` | FFmpeg executable: a name inside `toolsDirectory`, or an explicit path |
| `toolsDirectory` | `.tools` | Where downloaded helpers are kept, relative to the working directory |
| `autoDownload` | `false` | Download a helper that is missing |
| `inProcessAudio` | `true` | Decode, resample and encode in process where possible, instead of running FFmpeg |
| `lyricsSource` | `lrclib` | Lyrics service, or `none` |

Each helper is resolved in this order:

1. If the configured value contains a path separator, it is used exactly as
   written. A path that does not run is an error and never replaced, so a pinned
   build is never silently swapped for a download. This is how you use a system
   FFmpeg on purpose, for example `ffmpeg: /opt/homebrew/bin/ffmpeg`.
2. A copy in `toolsDirectory` is reused.
3. With `autoDownload` on, a build for the current platform is downloaded into
   `toolsDirectory` and used from then on.

A bare name is only ever a file inside `toolsDirectory`; it is not searched for on
`PATH`. So an FFmpeg that is already installed on the machine is ignored, and
`ffmpeg: ffmpeg` means `.tools/ffmpeg`. This keeps playback identical across
machines and stops a distribution build without libopus from surfacing as a
runtime failure. Naming `none` switches FFmpeg off instead of naming it, which is
how you run without it while `autoDownload` stays on for the helpers you do want.

FFmpeg and yt-dlp are both optional. If yt-dlp cannot be resolved, Golalink logs
a warning and continues with its native clients. If FFmpeg cannot be resolved it
logs a warning and starts anyway, because the in-process pipeline covers the
formats above; the tracks it does not cover then fail to load with a message
saying FFmpeg is needed. A configuration that names `none` logs that it was
switched off rather than that it was missing.

### Playing without FFmpeg

With `inProcessAudio` on, a track is decoded, resampled to 48 kHz stereo and
Opus-encoded inside the node whenever the format is one of the built-in ones.
FFmpeg still handles everything else, and the two paths produce the same audio:
the same volume arithmetic, the same mono upmix gain, the same surround fold and
the same 20 ms frame size.

| Handled in process | Sent to FFmpeg |
|---|---|
| MP3, WAV, FLAC at any bit depth, Ogg Vorbis, Ogg Opus, Ogg FLAC, AAC in an ADTS stream, AAC in an MP4 whose header is first, last or split across fragments, and Opus in WebM or Matroska | HE-AAC and other non-LC AAC profiles, Vorbis or AAC inside WebM, and any layout wider than eight channels |
| Tracks of up to eight channels, folded to stereo | Multichannel AAC, which the AAC decoder takes only as mono or stereo |
| All ten filters, including `equalizer`, `timescale` and `tremolo` | — |

Three cases are worth knowing about, because they depend on the file rather than
on the format:

- An MP4 whose header sits at the end, or whose samples are in fragments, is read
  by seeking. A source that cannot seek is retried with FFmpeg.
- Some surround Vorbis layouts are refused by the Vorbis decoder. Those tracks
  are retried with FFmpeg too.
- Multichannel AAC is left to FFmpeg, because the AAC decoder takes only mono and
  stereo.

A track that needs a filter or a format the in-process engine does not implement
is routed to FFmpeg before any of it is read, and a format that looks supported
but fails to open is retried once from the start of the stream. So the only
visible effect of turning this on is fewer processes; set it to `false` to prove a
deployment always uses FFmpeg.

### Reading container metadata

A direct `http://` or `https://` link is identified in process, so no probe
binary is installed and no subprocess is started per load. The reader takes the
duration and the title and artist tags from the container itself:

| Container | Duration | Tags |
|---|---|---|
| MP4, M4A, MOV | `mvhd` | `ilst` (`©nam`, `©ART`) |
| FLAC | `STREAMINFO` | Vorbis comment |
| WAV | `fmt ` and `data` | `LIST`/`INFO` |
| MP3 | Xing or Info frame count, else size and bitrate | ID3v2 `TIT2`, `TPE1` |
| Matroska, WebM | `Info` duration | `Info` title and the tag list |
| Ogg Opus, Ogg Vorbis | final page granule, when the whole file fits | comment header |
| ADTS AAC | frame count, when the whole file fits | none |

An MP3 duration taken from the frame count is a few tens of milliseconds longer
than the playing time, because it does not subtract the encoder padding that a
full decoder reports.

Throughput is bounded: at most 8 MiB is read, and Ogg and ADTS only resolve a
duration when the server declares a length that fits. A live stream therefore has
no duration, and loads with `isStream` true and `isSeekable` false rather than
delaying the load while more audio arrives. An MP4 whose `moov` box sits after
its audio loads the same way. Anything that is not a recognised audio container
is refused with `URL contains no supported audio stream`.

### Where the downloads come from

| Platform | Source |
|---|---|
| Windows (x64, ARM64) | [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) GPL builds |
| Linux (x64, ARM64) | [ffmpeg.martin-riedl.de](https://ffmpeg.martin-riedl.de), release channel, falling back to the snapshot channel |
| macOS (Intel, Apple silicon) | [ffmpeg.martin-riedl.de](https://ffmpeg.martin-riedl.de), as above |
| yt-dlp (all platforms) | [yt-dlp/yt-dlp](https://github.com/yt-dlp/yt-dlp) single-file release binaries, which need no Python |

Every build is checked against the provider's own SHA-256 listing where one is
published: BtbN ships `checksums.sha256` and yt-dlp ships `SHA2-256SUMS`. Both are
verified before the binary is installed. martin-riedl.de publishes no listing, so
those downloads are only protected by HTTPS. A download shorter than 1 MiB is
treated as a failure, which catches an error page served with a 200.

An archive is never left behind: it is fetched to a temporary directory, the
executable is copied out, and the temporary directory is removed when the process
exits. Only the named binary is unpacked, so the `ffprobe` that shares the BtbN
and martin-riedl archives is not written to disk. Nothing is downloaded when the
helper already exists, so a restart is cheap.

### Running without downloads

To prove a deployment never fetches anything, put FFmpeg in `toolsDirectory`
yourself and set `autoDownload: false`. Pointing `ffmpeg` at an explicit path has
the same effect, and a missing helper is then reported as a startup error naming
where it looked.

In the Docker image, FFmpeg is pinned to the distribution copy with
`GOLALINK_FFMPEG`, so the container starts without a download and runs a build
Debian has verified. yt-dlp is not in the image and is downloaded into the
`/app/.tools` volume on first start.

## Lyrics

`lyricsSource` names the services lyrics are fetched from. `lrclib` is a free,
open database that needs no account and returns timed lyrics, which is what lets
a client follow along line by line instead of showing a wall of text. Naming
`none` switches lyrics off and stops the node calling anything for them.

With `lrclib`, a track the database has nothing for is then asked of YouTube
Music's own catalogue, which holds the words for the uploads the database was
never given. The catalogue is asked twice about the same track: first as the
mobile app, which is answered with the timed lines, and then as the web app,
which is answered with the whole text as a single run. Timed lines win, because a
client can follow them, and the plain text is what a track gets when the
catalogue withholds the timings. Only a YouTube track can be asked about, since a
video id is the only handle the catalogue has, and it needs no account or key.

The `provider` field says which service answered: `lrclib` or `ytmusic`. An
answer from the catalogue also names whoever licensed it the words in
`plugin.source`, such as `Musixmatch`, so a client showing lyrics can credit it.

The routes are the ones the LavaLyrics plugin defines, so a client that already
shows lyrics from a Lavalink node running that plugin works unchanged:

| Route | Answers |
|---|---|
| `GET /v4/lyrics?track={encodedTrack}` | A track the client sends in full |
| `GET /v4/sessions/{sessionId}/players/{guildId}/track/lyrics` | Whatever a player is on |

Both answer `200` with the lyrics, `204` when the services have none for that
track, and `404` when there is no such player or nothing is playing. The payload
carries `sourceName`, `provider`, an optional `text`, `plugin` with the licensor
when the catalogue answered, and `lines` with a `timestamp` and a `duration` in
milliseconds.

A track is looked up once and remembered for an hour, so a client that asks twice
does not become two calls to a free public service. A lookup that failed is not
remembered, so a service that was briefly down is asked again rather than being
taken as a track with no lyrics. The title is cleaned before a database lookup,
because a video title carries parts that are not the song's name.

## Logging

| Option | Default | Values |
|---|---|---|
| `logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `logFormat` | `text` | `text`, `json` |
| `logColor` | `auto` | `auto`, `always`, `never` |
| `logDirectory` | `logs` | Directory for daily log files; empty disables file logging |

`text` is a single coloured line per record with the level padded to five
columns and values aligned. `json` is one JSON object per line for a collector,
and never coloured.

`logColor: auto` colours a real terminal and leaves a pipe or a file plain, so
`NO_COLOR` is respected and existing tooling is unaffected. `json` output is
never coloured whatever the setting.

With `logDirectory` set, records are also appended to
`<directory>/golalink-YYYY-MM-DD.log`, one file per day, rotated when the date
changes. Log files never contain colour escapes. The directory is created at
startup, and a directory that cannot be written is a startup failure rather than
silently lost logs; `-check` verifies it too. Set `logDirectory: ""` to log only
to the console, which is what the Docker Compose file does because Docker already
collects stdout.

Failures are always logged regardless of level: 4xx responses at `warn`, 5xx at
`error`, with the method, path, status, response size, source address and
duration. `debug` adds successful requests, session and player lifecycle, every
event sent to a client, and which resolver path a load took.

### Wordmark and console setup

Startup draws a `GOLALINK` wordmark in white and blue, followed by the version and
the Lavalink API version. It is skipped for `-check` and for `json` output so a
log stream stays parseable.

On Windows two things are enabled before anything is printed, because neither is
on by default: virtual terminal processing, so the colour escapes are interpreted
instead of printed, and the UTF-8 console code page, so non-ASCII track titles are
not mangled. If the console code page cannot be changed, the wordmark falls back
to a plain ASCII rendering.

## Sources

| Option | Default | Meaning |
|---|---|---|
| `youtubeNative` | `true` | Use the built-in InnerTube client for YouTube |
| `youtubeEndpoint` | `https://youtubei.googleapis.com` | InnerTube host |
| `youtubeMusicEndpoint` | `https://music.youtube.com` | Host used by `ytmsearch:` |
| `youtubeClientVersion` | empty | Override the InnerTube client version |
| `soundcloudNative` | `true` | Use the built-in SoundCloud client |
| `soundcloudApiEndpoint` | `https://api-v2.soundcloud.com` | SoundCloud API host |
| `soundcloudWebEndpoint` | `https://soundcloud.com` | Site read to discover a client id |
| `soundcloudClientId` | empty | Pin a client id instead of discovering one |
| `soundcloudClientIdTtlSeconds` | `21600` | How long a discovered client id is reused |
| `spotifyClientId` | empty | Spotify application client id |
| `spotifyClientSecret` | empty | Spotify application client secret |
| `spotifyRefreshToken` | empty | User authorisation refresh token |
| `spotifyMarket` | `US` | Market country code sent to Spotify |
| `deezerApiEndpoint` | `https://api.deezer.com` | Deezer public metadata host |
| `ytDlpEnabled` | `true` | Allow the yt-dlp fallback resolver |
| `ytDlp` | `yt-dlp` | yt-dlp executable: a name inside `toolsDirectory`, or an explicit path |

Spotify client id and secret must be set together. A refresh token additionally
requires the client id and secret, and any Spotify credential requires a market.
Turning `youtubeNative` or `soundcloudNative` off also requires the corresponding
endpoints to be set.

See [Sources](sources.md) for what each one does and where it stops.

## Filters

`filters` maps a filter name to whether clients may use it:

```yaml
filters:
  equalizer: false
  rotation: false
```

A filter that is absent stays enabled. Sending a disabled filter in a player
update is refused with `400` and `Following filters are disabled in the config:
<names>`, and the request does not create or modify the player. The available
names are `volume`, `equalizer`, `karaoke`, `timescale`, `tremolo`, `vibrato`,
`distortion`, `rotation`, `channelMix` and `lowPass`.

## Capacity

| Option | Default | Meaning |
|---|---|---|
| `maxPlayers` | `100` | Maximum concurrent players |
| `maxSessions` | `100` | Maximum concurrent sessions |
| `maxConcurrentLoads` | `8` | Loads resolved in parallel |
| `maxPlaylistTracks` | `100` | Tracks kept from one playlist |
| `searchResults` | `5` | Results returned for a search |
| `cacheEntries` | `1024` | Resolved track cache entries |
| `cacheTtlSeconds` | `300` | Cache entry lifetime |
| `loadTimeoutSeconds` | `30` | Timeout for one load request |
| `playerUpdateSeconds` | `5` | `playerUpdate` event interval |
| `statsSeconds` | `60` | `stats` event interval |
| `eventBuffer` | `256` | Events buffered per session while reconnecting |
| `opusBitrate` | `128000` | Opus bitrate in bit/s, 16000 to 256000 |

Every count and timeout above must be between 1 and 100000, and `opusBitrate`
between 16000 and 256000. A session whose buffer fills up is reclaimed instead of
growing without bound.

## Ratelimits

| Option | Default | Meaning |
|---|---|---|
| `ratelimits.enable` | `true` | Apply the per-route request limits |

Each route has its own token bucket and answers with `429` once it is exhausted.

Turning this off removes the throttling; protected routes still require the password.

## Webhook

| Option | Default | Meaning |
|---|---|---|
| `webhook.enable` | `false` | Post node events to an external endpoint |
| `webhook.url` | `` | Endpoint that receives the events |
| `webhook.stateLatencySeconds` | `5` | Latency between node-wide player state reports; `0` disables them |

With `webhook.enable` off there is no client at all, so every webhook operation is
skipped: no worker is started, an event is not encoded, and the node-wide player
state is not assembled. A URL written into the configuration while the webhook is
off is therefore never contacted, though it is still checked when the node starts.

Every player event (`TrackStartEvent`, `TrackEndEvent`, `TrackExceptionEvent`,
`TrackStuckEvent`, `WebSocketClosedEvent`) is posted to the URL, and so is every
change to the bot's own voice connection: `BotConnectEvent` when it joins a
channel and `BotDisconnectEvent` when it leaves one. A URL on `discord.com` (or
one whose path contains `/api/webhooks/`) is sent a Discord embed; any other
endpoint is sent the event itself as JSON.

A `BotDisconnectEvent` carries the `channelId` the bot left and a `reason` of
`closed` (the voice socket dropped, reported with the close `code` and
`byRemote`), `replaced` (the bot was moved to another channel or given a new
voice state) or `destroyed` (the player was destroyed, including when the client
disconnects). The voice token, the voice session and the voice endpoint are
never included, and a connection that never came up reports nothing. Both events
reach the owning client as well, like every other event.

A client only ever receives the `playerUpdate` of its own session. The webhook
carries the whole node instead: `stateLatencySeconds` is how far apart two of
those reports are, and a report asked for sooner than that after the last one is
cancelled rather than queued. One is posted as soon as the node starts, so a
restart shows up in the channel before anything is playing, and the node asks for
another whenever its set of players changes or an event goes out. A busy node
therefore posts one report on that interval, and a node that falls idle goes
quiet after the report that shows the last player leaving. Setting the latency to
`0` turns the reports off. It is not a Lavalink event, so it has its own shape,
and a Discord endpoint renders it as a `Player State` embed with one field per
player.

The report is written for a channel that may be read by anyone the endpoint is
shared with, so it is deliberately narrow: no session or user identifier, no
encoded track, no track URI and no voice credentials. Only what describes
playback is included.

```json
{
  "type": "PlayerStateUpdate",
  "timestamp": 1735689600000,
  "sessions": 1,
  "players": 2,
  "playingPlayers": 1,
  "states": [
    {
      "guildId": "5678...",
      "title": "Never Gonna Give You Up",
      "author": "Rick Astley",
      "source": "youtube",
      "length": 215000,
      "position": 42000,
      "volume": 100,
      "paused": false,
      "connected": true,
      "ping": 23
    }
  ]
}
```

`title`, `author`, `source` are empty and `length` is `0` when a player has
nothing loaded; `position` is where the track is in milliseconds, `volume` is a
percentage, and `ping` is the latency to the voice server.

Delivery runs on a single background worker so a slow endpoint never stalls
playback. A delivery that fails, or an event that cannot be queued, is logged at
`FATAL` and stops the node: a configured endpoint is treated as part of the
deployment. Discord rate limits a webhook, so raise `stateLatencySeconds` when a
node carries a lot of events, or set it to `0` and rely on events alone.

## Security

| Option | Default | Meaning |
|---|---|---|
| `allowPrivateNetworks` | `false` | Allow sources that resolve to private or special addresses |

Leaving this off blocks SSRF through direct HTTP sources: every dial and redirect
re-checks the resolved address. FFmpeg itself is only ever given a pipe, never a
URL or a local file path. yt-dlp is a separate resolver and its own internal
requests are not covered by this check.

## Environment variables

Environment variables override the configuration file. Set a variable to an empty
string to force the empty value.

| Variable | Option |
|---|---|
| `GOLALINK_ADDRESS` | `address` |
| `GOLALINK_PASSWORD` | `password` |
| `GOLALINK_FFMPEG` | `ffmpeg` |
| `GOLALINK_TOOLS_DIRECTORY` | `toolsDirectory` |
| `GOLALINK_LOG_LEVEL` | `logLevel` |
| `GOLALINK_LOG_FORMAT` | `logFormat` |
| `GOLALINK_LOG_DIRECTORY` | `logDirectory` |
| `GOLALINK_LOG_COLOR` | `logColor` |
| `GOLALINK_YTDLP` | `ytDlp` |
| `GOLALINK_YOUTUBE_ENDPOINT` | `youtubeEndpoint` |
| `GOLALINK_YOUTUBE_MUSIC_ENDPOINT` | `youtubeMusicEndpoint` |
| `GOLALINK_YOUTUBE_CLIENT_VERSION` | `youtubeClientVersion` |
| `GOLALINK_SOUNDCLOUD_API_ENDPOINT` | `soundcloudApiEndpoint` |
| `GOLALINK_SOUNDCLOUD_WEB_ENDPOINT` | `soundcloudWebEndpoint` |
| `GOLALINK_SOUNDCLOUD_CLIENT_ID` | `soundcloudClientId` |
| `GOLALINK_DEEZER_API_ENDPOINT` | `deezerApiEndpoint` |
| `SPOTIFY_CLIENT_ID` | `spotifyClientId` |
| `SPOTIFY_CLIENT_SECRET` | `spotifyClientSecret` |
| `SPOTIFY_REFRESH_TOKEN` | `spotifyRefreshToken` |
| `SPOTIFY_MARKET` | `spotifyMarket` |
| `GOLALINK_WEBHOOK_URL` | `webhook.url` |

Boolean options accept `0` or `false` for off and anything else for on:

| Variable | Option |
|---|---|
| `GOLALINK_YOUTUBE_NATIVE` | `youtubeNative` |
| `GOLALINK_SOUNDCLOUD` | `soundcloudNative` |
| `GOLALINK_YTDLP_ENABLED` | `ytDlpEnabled` |
| `GOLALINK_AUTO_DOWNLOAD` | `autoDownload` |
| `GOLALINK_IN_PROCESS_AUDIO` | `inProcessAudio` |
| `GOLALINK_PRIVATE_NETWORKS` | `allowPrivateNetworks` |
| `GOLALINK_RATELIMITS_ENABLE` | `ratelimits.enable` |
| `GOLALINK_WEBHOOK_ENABLED` | `webhook.enable` |
