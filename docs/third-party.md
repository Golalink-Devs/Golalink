# Third-party components

Golalink is implemented independently against public protocol documentation. No
Lavalink or NodeLink server source was copied into this project.

Exact versions and checksums are in `go.mod` and `go.sum`. The licences below come
from each module's own LICENSE file.

## Direct dependencies

| Component | Version | Used for | Licence |
|---|---|---|---|
| github.com/disgoorg/disgo | v0.19.6 | Discord voice gateway and UDP transport | Apache-2.0 |
| github.com/disgoorg/godave | v0.2.0 | DAVE interfaces | Apache-2.0 |
| github.com/disgoorg/snowflake/v2 | v2.0.3 | Discord IDs | Apache-2.0 |
| github.com/gorilla/websocket | v1.5.3 | Client control WebSocket | BSD-2-Clause |
| github.com/hajimehoshi/go-mp3 | v0.3.4 | MP3 decoding without FFmpeg | Apache-2.0 |
| github.com/jfreymuth/oggvorbis | v1.0.6-0.20260818085226-94cd36b10b9d | Vorbis decoding without FFmpeg | MIT |
| github.com/thomas-vilte/dave-go | v0.5.1 | DAVE implementation | Apache-2.0 |
| github.com/tphakala/go-aac | v0.7.0 | AAC-LC decoding without FFmpeg | **LGPL-2.1-or-later** |
| github.com/tphakala/go-flac | v1.1.0 | FLAC decoding without FFmpeg | MIT |
| github.com/tphakala/go-opus | v1.1.0 | Opus decoding and encoding without FFmpeg | BSD-3-Clause |
| github.com/tphakala/go-wav | v1.1.0 | WAV decoding without FFmpeg | MIT |
| golang.org/x/sync | v0.23.0 | Request coalescing | BSD-3-Clause |
| golang.org/x/sys | v0.47.0 | Windows console setup and CPU sampling | BSD-3-Clause |
| gopkg.in/yaml.v3 | v3.0.1 | YAML configuration | MIT and Apache-2.0 |

## Indirect dependencies

| Component | Version | Licence |
|---|---|---|
| github.com/disgoorg/json/v2 | v2.0.0 | Apache-2.0 |
| github.com/disgoorg/omit | v1.0.0 | Apache-2.0 |
| github.com/jfreymuth/vorbis | v1.0.3-0.20230305115442-bba79ca271fb | MIT |
| github.com/klauspost/compress | v1.18.4 | Apache-2.0, MIT and BSD-3-Clause |
| github.com/sasha-s/go-csync | v0.0.0-20240107134140-fcbab37b09ad | Apache-2.0 |
| github.com/thomas-vilte/mls-go | v1.6.0 | MIT |
| github.com/tphakala/simd | v1.9.0 | MIT |
| golang.org/x/crypto | v0.54.0 | BSD-3-Clause |

Every module above except `go-aac` is permissively licensed (Apache-2.0, MIT, BSD
or ISC), so it can be combined with GPL-3.0 code. `go-aac` is the one exception
and is covered below.

## Pinned commits

The two Vorbis modules are pinned to commits rather than to releases, because the
fixes this project relies on are not in a release yet:

- `oggvorbis` v1.0.5 panics on a valid Ogg page that carries no segments, which a
  crafted file reaches through the ordinary open path. Fixed upstream in
  `94cd36b`.
- `vorbis` v1.0.2 builds a codebook lookup with `math.Pow` in a way that rounds
  down on some platforms, which would misread the codebook on an arm64 build.
  Fixed upstream in `bba79ca`.

Both are a handful of lines. `go.mod` pins the commits and `go.sum` carries their
checksums, so builds stay reproducible; moving to a release is a matter of
updating both once one contains the fixes.

## LGPL components

`github.com/tphakala/go-aac` is **LGPL-2.1-or-later** and is the only
non-permissive dependency. It decodes AAC-LC, which is what MP4 and M4A audio
tracks normally hold, and it is what lets those play without FFmpeg. The other
AAC profiles it does not support, HE-AAC among them, still go to FFmpeg.

The LGPL permits a work to use the library under terms of the user's choosing
provided the library itself stays under the LGPL and can be replaced. Section 3
also allows the library to be taken under GPL-2.0-or-later, and GPL-2.0-or-later
code may be used under GPL-3.0, so the combination is allowed. A Go build links
it statically, which is the case section 6 covers: it asks that a recipient be
able to relink the work with a modified version of the library. That is satisfied
here because the whole of Golalink is GPL-3.0 with its source public, `go.mod`
pins `go-aac` to an exact version with checksums in `go.sum`, and anyone can drop
in a modified `go-aac` and rebuild with `go build ./cmd/golalink`.

The unmodified source of `go-aac` is at
<https://github.com/tphakala/go-aac>. Its licence text ships inside the module in
the Go module cache and is reproduced in any binary's dependency notices.

## External executables

FFmpeg and yt-dlp are separate programs, not linked libraries. Their
licences depend on the build and release you end up running:

- FFmpeg is LGPL-2.1-or-later by default and GPL-2.0-or-later when built with
  `--enable-gpl`, which is the case for every build Golalink downloads. Those
  builds are downloaded at runtime and are not distributed with this project.
- yt-dlp is Unlicense, and its release binaries bundle third-party components
  whose notices are included in the release.

Check the distribution you use before redistributing its binaries. The Dockerfile
installs FFmpeg from the Debian archive.

## External services

Lyrics are fetched from public services when `lyricsSource` names one. Neither
service is affiliated with this project, and neither is redistributed with it:

- **lrclib.net** is a free, open lyrics database that needs no account.
- **YouTube Music's catalogue** is asked for the tracks lrclib has nothing for,
  through the same InnerTube API the node already uses to resolve YouTube audio.
  The words it returns are licensed to YouTube by their publisher, and the answer
  names the licensor (`Musixmatch`, `LyricFind` and others). Golalink passes that
  name on in the `plugin.source` field of the lyrics payload, so a client that
  displays the words can credit it.

## References

Protocol and architecture references:
[Lavalink](https://github.com/lavalink-devs/Lavalink),
[NodeLink](https://github.com/PerformanC/NodeLink) and
[Lavaplayer](https://github.com/lavalink-devs/lavaplayer). Golalink is not an
official release of, or endorsed by, any of those projects, and does not claim to
be.

## This project

Golalink is licensed under the **GNU General Public License v3.0**; the full text
is in [LICENSE](../LICENSE). Copyright (C) 2026 Golalink contributors.

Because the GPL is a strong copyleft licence, anyone who distributes a modified
version has to offer the corresponding source to whoever receives it. The
unmodified source is already public at <https://github.com/Golalink/Golalink>, so
an unmodified deployment satisfies that on its own. Running the node as a network
service does not by itself create any obligation, because the GPL has no
equivalent of the AGPL's section 13.
