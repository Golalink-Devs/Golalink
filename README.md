# Golalink

[![ci](https://github.com/Golalink/Golalink/actions/workflows/ci.yml/badge.svg)](https://github.com/Golalink/Golalink/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Golalink/Golalink?sort=semver)](https://github.com/Golalink/Golalink/releases/latest)
[![nightly](https://github.com/Golalink/Golalink/actions/workflows/nightly.yml/badge.svg)](https://github.com/Golalink/Golalink/actions/workflows/nightly.yml)
[![container](https://img.shields.io/badge/ghcr.io-golalink-2496ED?logo=docker&logoColor=white)](https://github.com/Golalink/Golalink/pkgs/container/golalink)
[![licence](https://img.shields.io/github/license/Golalink/Golalink)](LICENSE)
[![go](https://img.shields.io/github/go-mod/go-version/Golalink/Golalink)](go.mod)

**English** · [繁體中文](README.zh-TW.md)

A standalone Discord audio node written in Go, speaking the Lavalink v4 REST and
WebSocket protocol.

Golalink talks to Discord's voice gateway itself. Your bot stays on the main
gateway, forwards the `VOICE_STATE_UPDATE` and `VOICE_SERVER_UPDATE` payloads, and
controls playback over the Lavalink API. No Java, no CGO, and no Discord bot
token. FFmpeg and yt-dlp are optional.

## Features

- Lavalink v4 track loading, player and session management, `info` and `stats`.
- WebSocket events: `ready`, `stats`, `playerUpdate`, track start/end/exception/stuck, voice closed.
- Session resuming, with events buffered while a client is reconnecting.
- 48 kHz stereo Opus at 20 ms per packet: volume, pause, seek, stop, `endTime`, `noReplace`, `userData`.
- Discord voice gateway, UDP transport and DAVE through DisGo.
- MP3, WAV, FLAC at any bit depth, Ogg Vorbis, Ogg Opus, Ogg FLAC, AAC in ADTS or MP4, and Opus in WebM decoded, resampled and Opus-encoded in process.
- Surround tracks of up to eight channels, folded to stereo by the same mix FFmpeg uses.
- All ten standard filters run in process, `timescale` included.
- Lyrics over the LavaLyrics routes, timed, from LRCLIB and from YouTube Music's own catalogue for what it has nothing for.
- Prometheus metrics, structured console and file logs, graceful shutdown.

## Install

### Prebuilt binaries

Every release carries an archive per platform, plus `checksums.txt`:

```sh
curl -LO https://github.com/Golalink/Golalink/releases/latest/download/golalink_v0.1.0_linux_amd64.tar.gz
tar -xzf golalink_v0.1.0_linux_amd64.tar.gz
./golalink_v0.1.0_linux_amd64/golalink
```

| Platform | Architectures |
|---|---|
| Windows | x86, x86-64, ARM64 |
| Linux | x86, x86-64, ARMv6, ARMv7, ARM64, LoongArch64, MIPS, MIPS LE, MIPS64, MIPS64 LE, POWER8, POWER8 LE, RISC-V 64, s390x |
| macOS | x86-64, Apple silicon |
| Container | `linux/amd64`, `linux/arm64`, `linux/arm/v7`, `linux/386`, `linux/ppc64le`, `linux/s390x` |

A build from the latest commit on the default branch is published as a nightly
prerelease. It is not a release: use a tagged version for anything you depend on.

### Container

```sh
docker run -d --name golalink \
  -p 127.0.0.1:2333:2333 \
  -e GOLALINK_PASSWORD=a-long-random-password \
  -v golalink-tools:/app/.tools \
  ghcr.io/golalink/golalink:latest
```

The image already contains FFmpeg. yt-dlp is downloaded into the volume on first
start and reused afterwards. It runs as a non-root user on a read-only
filesystem.

### From source

```sh
go build -trimpath -o bin/golalink ./cmd/golalink
```

Go 1.26.5 or newer is required. See [Requirements](#requirements) for what the
node needs at runtime.

## Requirements

Golalink decodes, filters and Opus-encodes in process, so a running node needs no
external program for the formats it covers. FFmpeg (built with libopus) is the
fall back for the few formats it does not, and is read from `.tools` rather than
from `PATH`, so every node runs the same build whatever a machine happens to have
installed. Container metadata is parsed in process, so there is no probe binary to
install.

Both helpers are optional:

- Without FFmpeg the node starts and plays everything the in-process pipeline
  covers. Only HE-AAC, multichannel AAC, and Vorbis or AAC inside WebM fail to
  load. Naming `none` switches it off and stops it being downloaded.
- yt-dlp is only a fallback, because YouTube and SoundCloud are handled natively.
  It is downloaded on first start when `autoDownload` is on.

See [Configuration](docs/configuration.md#media-tools).

## Running it

Every platform follows the same three steps: build or unpack, set a password,
start.

### Windows

```powershell
go build -trimpath -o bin\golalink.exe .\cmd\golalink
$env:GOLALINK_PASSWORD = 'a-long-random-password'
.\bin\golalink.exe
```

### Linux and macOS

```sh
go build -trimpath -o bin/golalink ./cmd/golalink
export GOLALINK_PASSWORD='a-long-random-password'
./bin/golalink
```

On macOS a downloaded FFmpeg is unsigned, so Gatekeeper may refuse to run it. If
that happens, install FFmpeg with Homebrew and point the configuration at it —
`ffmpeg: /opt/homebrew/bin/ffmpeg` — because a bare name only ever means "inside
`.tools`". Naming `none` is also fine, if you do not need those formats.

### Docker

```sh
export GOLALINK_PASSWORD='a-long-random-password'
docker compose up -d
docker compose logs -f
```

Golalink listens on `127.0.0.1:2333` by default. Set
`GOLALINK_ADDRESS=0.0.0.0:2333` to accept remote clients, and put TLS or an
address restriction in front of it before exposing it.

## Configuration

`config.example.yaml` lists every option with a one-line comment. Copy it and
change what you need:

```sh
cp config.example.yaml config.yaml
```

```powershell
Copy-Item config.example.yaml config.yaml
```

Golalink looks for `config.yaml`, `config.yml` or `config.json` in the working
directory when `-config` is not given, so an unpacked release starts with no
arguments. Environment variables always override the file. Field names are
matched strictly: a typo stops startup instead of being silently ignored.

`golalink -check` validates the configuration and the media helpers, then exits
without listening.

[Configuration](docs/configuration.md) documents every option and environment
variable.

## Connecting a bot

Use a client that supports the v4.2 voice `channelId`, and point it at Golalink:

```http
Authorization: your-node-password
User-Id: your-discord-bot-user-id
Client-Name: your-client/1.0.0
```

Connect to `/v4/websocket`, read `ready.sessionId`, then load a track:

```http
GET /v4/loadtracks?identifier=ytsearch%3Anever%20gonna%20give%20you%20up
Authorization: your-node-password
```

Take the `encoded` value from one result and send it as `track.encoded` in
`PATCH /v4/sessions/{sessionId}/players/{guildId}`, together with the `voice`
object built from your bot's voice state and voice server events.

Two details trip people up. A search result cannot be used as a single track
`identifier`, so pass the `encoded` value instead. And playback has started when
`TrackStartEvent` arrives, not when the REST call returns.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /version` | Protocol version |
| `GET /healthz` | Liveness check, no authentication |
| `GET /v4/info` | Enabled sources, version, filters |
| `GET /v4/stats` | Lavalink-shaped statistics |
| `GET /v4/lyrics` | Lyrics for an encoded track |
| `GET /v4/sessions/{sessionId}/players/{guildId}/track/lyrics` | Lyrics for what a player is on |
| `GET /metrics` | Prometheus metrics, node password required |

`/v4/metrics` serves the same metrics as `/metrics`.

`/healthz` and `/version` are deliberately unauthenticated so a container
orchestrator can probe the process. Everything else needs the node password.

## Project layout

```text
cmd/golalink/       entry point
internal/config/    configuration loading and validation
internal/logging/   console and file logs, startup wordmark
internal/tools/     media helper lookup and downloads
internal/server/    HTTP, WebSocket, sessions, metrics, lyrics routes
internal/player/    playback state, events and cancellation
internal/source/    YouTube, SoundCloud, Spotify, Deezer, HTTP, yt-dlp
internal/audio/     FFmpeg pipeline, filters, Ogg and Opus framing
internal/audio/pure/        in-process decoder, resampler and Opus encoder
internal/audio/dispatch/    chooses between the two audio engines
internal/lyrics/    lyrics providers and the LRC parser
internal/voice/     DisGo and DAVE transport
internal/protocol/  Lavalink JSON types and the track codec
internal/netguard/  outbound URL, DNS and IP restrictions
internal/system/    CPU sampling
docs/               documentation
```

## Building

`.github/workflows/build.yml` holds the target list, and
`.github/workflows/ci.yml` compiles all of it on every change, so a target is
never found broken only when a tag is pushed. `release.yml` publishes a tagged
release to GitHub Releases and a multi-architecture image to `ghcr.io`;
`nightly.yml` does the same for the latest commit on a schedule.

To reproduce a release build locally:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-s -w -X golalink/internal/server.Version=v0.1.0" \
  -o bin/golalink ./cmd/golalink
```

## Quick Building

[Linux](/build/build-linux.sh)

[MacOS](/build/build-macos.sh)

[Windows](/build/build-windows.bat)

## Documentation

- [Configuration](docs/configuration.md) — every option and environment variable
- [Sources](docs/sources.md) — how each source is resolved
- [Compatibility](docs/compatibility.md) — Lavalink v4 coverage and known differences
- [Third-party](docs/third-party.md) — dependencies and their licences

## Licence

GNU General Public License v3.0, full text in [LICENSE](LICENSE).
Copyright (C) 2026 Golalink contributors.

The GPL is a strong copyleft licence: if you distribute a modified Golalink, you
have to pass the same freedoms on and make the corresponding source available to
whoever receives it. Running it unmodified, or only for yourself, carries no
extra obligation. The unmodified source is public at
<https://github.com/Golalink/Golalink>.

Third-party dependencies keep their own licences, which are compatible with the
GPL; see [Third-party](docs/third-party.md).
