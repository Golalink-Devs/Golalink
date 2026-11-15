# Sources

Golalink resolves a `loadtracks` identifier through the first source that claims
it. YouTube and SoundCloud have native clients, so neither needs yt-dlp. Spotify
and Deezer supply metadata only: the track is then matched to a YouTube source
and played from there.

## Identifiers

| Identifier | Source |
|---|---|
| `ytsearch:<query>` | YouTube search |
| `ytmsearch:<query>` | YouTube Music search |
| `scsearch:<query>` | SoundCloud search |
| `spsearch:<query>` | Spotify search |
| `sprec:<id>`, `spprev:<id>` | Spotify generated playlists |
| `https://www.youtube.com/watch?v=<id>` | YouTube |
| `https://soundcloud.com/...` | SoundCloud |
| `https://open.spotify.com/...` | Spotify |
| `https://www.deezer.com/...` | Deezer |
| any other `http(s)://` | Direct HTTP stream |

`spsearch`, `sprec` and `spprev` exist; `sptrack:`, `spalbum:`, `spplaylist:` and
`spartist:` do not. A bare 11-character YouTube video ID is accepted.

## YouTube

Resolution uses a native InnerTube client: plain HTTPS `POST` requests to
`youtubei.googleapis.com`, with all client identity in the request body. Search,
single videos, playlists and mixes are all covered, and no JavaScript runtime and
no player script are involved.

Playlists are read through `browse` with continuation tokens, page by page. Both
item shapes YouTube currently alternates between are parsed, and both
continuation shapes are followed. Duplicates across pages are removed, and if a
continuation fails the pages already fetched are kept rather than the whole load
failing. `maxPlaylistTracks` truncates the result and sets `pluginInfo.truncated`.

The audio URL is short-lived, so it is resolved at playback time and never cached
as a playable URL.

**Formats.** The usable YouTube format is the muxed progressive one, about
128 kbps AAC, and the audio track is taken out of it while it plays. The
audio-only adaptive formats are only handed out alongside a proof-of-origin
token or through SABR, and neither is implemented, so the native client does not
depend on them.

**Client versions.** YouTube rejects older InnerTube client versions as they age,
so the native client tries its configured clients in turn and reports each
client's reason if all of them fail. `youtubeClientVersion` overrides the version
of the first client only.

**Search with no results.** YouTube matches multi-word queries strictly, so a rare
title can return nothing at all. When that happens the query is retried with
simplified variants, in order: the query itself, then with bracketed parts and
filler words such as `audio`, `lyrics`, `official`, `mv` or `cover` removed, then
the longer half of a `-` separated title, then the first eight words. At most
three variants are tried, each only after the previous one returned nothing.
`ytmsearch:` uses the same logic.

## YouTube Music

`ytmsearch:` calls the YouTube Music host with the `WEB_REMIX` client and a songs
filter, and parses `musicResponsiveListItemRenderer` entries. Rows that are not
tracks, such as albums and artists, are discarded. If the music search fails, it
falls back to an ordinary YouTube search. Only the search step uses the music
host; the resulting video IDs are played through the normal path.

## SoundCloud

A native client against the public `api-v2` endpoints, with no account and no API
key. Track search reads `/search/tracks`, single tracks read `/tracks/{id}`, and
links are normalised through `/resolve`.

Each track lists media transcodings, and the protocol is read from the transcoding
`format` object rather than the top-level field, which is empty. A progressive MP3
is preferred, because it can be read straight from a pipe and no HLS handling is
needed. The CDN URL is time limited, so it is resolved again at every playback
start and never cached.

Sets are the awkward case: `/resolve` fills in only a few of the entries and
leaves the rest as stubs with just a kind and an ID. Each stub is fetched
individually, a few at a time, so a set resolves completely.

**Client id.** The API needs a `client_id`, which is scraped from the JavaScript
bundles the site references. Only the first 64 KiB of each bundle is fetched, and
the id is cached for `soundcloudClientIdTtlSeconds`, after which it is scraped
again; a `401` or `403` also drops the cached id. A full bundle download costs tens
of seconds, which is why the head-only read is used. The front page occasionally
serves a stub without any bundle, in which case `/discover` is tried before giving
up. Setting `soundcloudClientId` skips scraping entirely.

## Spotify

Spotify is used for metadata. The API is called with Client Credentials, and a
user refresh token is optional. Tracks, albums, playlists, artists and search are
supported.

Playlists are read from `/items` first and `/tracks` second, because newly created
Spotify applications are only allowed the newer endpoint while older ones are only
allowed the older one; both are tried once and both reasons are reported if both
fail. Albums and playlists are paginated up to six pages, so 600 playlist tracks
and 300 album tracks; local files and podcast episodes inside a playlist are
skipped. `spotify:artist:ID` and artist URLs read `/artists/{id}` and then
`/artists/{id}/top-tracks`, returning `loadType: playlist` with
`pluginInfo.type: artist`.

Generated playlists whose ID begins with `37i9dQZ` cannot be read with client
credentials. They are refused immediately with an explanation and no request is
sent. A user refresh token bypasses that check.

**Playback is a mirror.** A Spotify track is not downloaded or decrypted. Its
title, artist and duration are matched against a YouTube search, using the query
form `author - title audio`, and the YouTube audio is what plays. Title and artist
similarity plus a duration window decide the match, and a track with no acceptable
match fails with an explicit error rather than playing the wrong thing.

## Deezer

Deezer is also metadata only, read from the public API without credentials. Both
pagination shapes are supported: albums nest their tracks under `tracks`, while
playlists use a flat `data` array with an absolute `next` link, which is only
followed when its host matches the configured endpoint.

`dzsearch:` and `dzisrc:` are refused with an explanation. Deezer's public search
returns a result count with an empty data array when there is no account, so
sending the request would only produce a confusing empty result. `link.deezer.com`
short links are not expanded either; use a full `deezer.com` URL.

Nothing on Deezer is streamed or decrypted. Upstream LavaSrc requires an `arl`
cookie and a master decryption key for that, and this project does not do it on
purpose. Like Spotify, a Deezer item is matched to a YouTube source.

## Direct HTTP streams

Any other `http(s)://` URL is fetched by Go's HTTP client and handed to the audio
pipeline, which decodes it in process when the container is one it covers and
falls back to FFmpeg when it is not. There is no manifest handling: an HLS or DASH
URL, a DRM-protected stream or member-only content will not work, and a URL that
points at a playlist rather than a single media stream is not expanded.

Because Go fetches the body and pipes it onwards, FFmpeg never receives a URL or a
file path and cannot be pointed at the local filesystem. `allowPrivateNetworks`
decides whether private and special addresses may be reached, and every dial and
redirect is re-checked.

Seeking inside such a stream works by decoding from the beginning up to the target
position, which can be slow for a long file.

## yt-dlp

yt-dlp is a fallback, not a dependency. It is used for SoundCloud when the native
path fails, and for the generic extractors that have no native client here, such as
Bandcamp. It runs with `--ignore-config`, no download, and a socket timeout, and it
is pointed at Node.js as a JavaScript runtime when Node.js is on `PATH`.

Set `ytDlpEnabled: false` and Golalink will not start another process for
resolution at all. YouTube and SoundCloud keep working through their native
clients.

## Playback robustness

Media CDNs rate limit, expire URLs and drop connections, so the read path is built
for it rather than assuming a clean stream.

Opening a stream retries with up to three attempts. A `403`, `404`, `410`, `429`
or any `5xx` is treated as retryable and the track is resolved again to get a fresh
URL; `401` and `400` are permanent and fail immediately.

Once playing, a dropped connection is continued rather than ending the track: the
body asks for `bytes=<bytes already delivered>-` and can do so up to three times,
waiting 250 ms, then 500 ms, then 750 ms. Bytes already read are always delivered
first, so a failure at the very start still fails quickly. If the source ignores
the range and returns the whole stream, the already-delivered prefix is discarded so
the bytes stay contiguous. If every attempt fails, the original read error is what
the client sees.

If a server sends a complete `206 Partial Content` segment that ends before the
end of the file, the next byte range is requested immediately. These requests
do not use up the retries for dropped connections.

A URL that expires in the middle of a track is not re-resolved by the continuation
path, so that track fails. Playback fetches always send `Range: bytes=0-`; the
probe used while loading deliberately does not.

If playback startup or reading the next frame takes more than ten seconds, the
player cancels the media request and decoder and emits `TrackStuckEvent`. This
applies to both the in-process decoder and FFmpeg. The watchdog is idle while
the player is waiting to unpause.
