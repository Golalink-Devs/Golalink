# Golalink

[![ci](https://github.com/Golalink/Golalink/actions/workflows/ci.yml/badge.svg)](https://github.com/Golalink/Golalink/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Golalink/Golalink?sort=semver)](https://github.com/Golalink/Golalink/releases/latest)
[![nightly](https://github.com/Golalink/Golalink/actions/workflows/nightly.yml/badge.svg)](https://github.com/Golalink/Golalink/actions/workflows/nightly.yml)
[![container](https://img.shields.io/badge/ghcr.io-golalink-2496ED?logo=docker&logoColor=white)](https://github.com/Golalink/Golalink/pkgs/container/golalink)
[![licence](https://img.shields.io/github/license/Golalink/Golalink)](LICENSE)
[![go](https://img.shields.io/github/go-mod/go-version/Golalink/Golalink)](go.mod)

[English](README.md) · **繁體中文**

用 Go 寫的獨立 Discord 音訊節點，實作 Lavalink v4 的 REST 與 WebSocket 協定。

Golalink 自己跟 Discord 的語音閘道溝通。你的機器人留在主閘道上，把
`VOICE_STATE_UPDATE` 和 `VOICE_SERVER_UPDATE` 轉過來，再用 Lavalink API 控制播放。
不需要 Java、不需要 CGO、不需要 Discord 機器人權杖。FFmpeg 和 yt-dlp 都是選配。

## 功能

- Lavalink v4 的曲目載入、播放器與 session 管理、`info` 與 `stats`。
- WebSocket 事件：`ready`、`stats`、`playerUpdate`、曲目開始／結束／例外／卡住、語音連線關閉。
- Session 續用，客戶端重連期間事件會先緩衝。
- 48 kHz 立體聲 Opus、每包 20 ms：音量、暫停、seek、停止、`endTime`、`noReplace`、`userData`。
- 透過 DisGo 接上 Discord 語音閘道、UDP 傳輸與 DAVE。
- MP3、WAV、任意位元深度的 FLAC、Ogg Vorbis、Ogg Opus、Ogg FLAC、ADTS 或 MP4 裡的 AAC、
  WebM 裡的 Opus，全部在行程內解碼、重採樣並編成 Opus。
- 最多八聲道的環繞曲目，用跟 FFmpeg 相同的混音折成兩聲道。
- 十個標準濾鏡全部在行程內執行，包含 `timescale`。
- 走 LavaLyrics 路由的逐行歌詞，來源是 LRCLIB，LRCLIB 沒有的曲目再向 YouTube Music 自己的目錄要。
- Prometheus 指標、結構化主控台與檔案日誌、正常關閉。

## 安裝

### 現成執行檔

每個 release 都附各平台的壓縮檔，以及 `checksums.txt`：

```sh
curl -LO https://github.com/Golalink/Golalink/releases/latest/download/golalink_v0.1.0_linux_amd64.tar.gz
tar -xzf golalink_v0.1.0_linux_amd64.tar.gz
./golalink_v0.1.0_linux_amd64/golalink
```

| 平台 | 架構 |
|---|---|
| Windows | x86、x86-64、ARM64 |
| Linux | x86、x86-64、ARMv6、ARMv7、ARM64、LoongArch64、MIPS、MIPS LE、MIPS64、MIPS64 LE、POWER8、POWER8 LE、RISC-V 64、s390x |
| macOS | x86-64、Apple silicon |
| 容器 | `linux/amd64`、`linux/arm64`、`linux/arm/v7`、`linux/386`、`linux/ppc64le`、`linux/s390x` |

預設分支最新提交的建置會以 nightly 預先發行版發布。那不是正式版本，要依賴的話請用有 tag 的版本。

### 容器

```sh
docker run -d --name golalink \
  -p 127.0.0.1:2333:2333 \
  -e GOLALINK_PASSWORD=a-long-random-password \
  -v golalink-tools:/app/.tools \
  ghcr.io/golalink/golalink:latest
```

映像裡已經有 FFmpeg。yt-dlp 會在第一次啟動時下載到 volume 裡，之後重複使用。
容器以非 root 使用者、在唯讀檔案系統上執行。

### 從原始碼建置

```sh
go build -trimpath -o bin/golalink ./cmd/golalink
```

需要 Go 1.26.5 或更新。執行時需要什麼見[執行需求](#執行需求)。

## 執行需求

Golalink 在行程內解碼、套濾鏡並編成 Opus，所以它支援的格式不需要任何外部程式。
FFmpeg（需含 libopus）是它不支援的那少數格式的備援，而且只從 `.tools` 讀取、
不查 `PATH`，這樣不管機器上裝了什麼，每個節點跑的都是一樣的建置。容器中介資料
也是在行程內解析的，所以不需要安裝任何探測用的執行檔。

兩個 helper 都是選配：

- 沒有 FFmpeg，節點照樣啟動，行程內管線涵蓋的格式全部能播。只有 HE-AAC、多聲道
  AAC、以及 WebM 裡的 Vorbis 或 AAC 會載入失敗。把值寫成 `none` 就是明確關掉，
  也不會去下載。
- yt-dlp 只是備援，因為 YouTube 和 SoundCloud 都走原生。`autoDownload` 開著時
  會在第一次啟動下載。

見[設定](docs/configuration.md#media-tools)。

## 啟動

每個平台都是同樣三步：建置或解壓、設密碼、啟動。

### Windows

```powershell
go build -trimpath -o bin\golalink.exe .\cmd\golalink
$env:GOLALINK_PASSWORD = 'a-long-random-password'
.\bin\golalink.exe
```

### Linux 與 macOS

```sh
go build -trimpath -o bin/golalink ./cmd/golalink
export GOLALINK_PASSWORD='a-long-random-password'
./bin/golalink
```

macOS 上抓下來的 FFmpeg 沒有簽章，Gatekeeper 可能會擋。遇到就用 Homebrew 裝一份，
再把設定指過去 —— `ffmpeg: /opt/homebrew/bin/ffmpeg` —— 因為單純的名字只代表
「`.tools` 裡的檔案」。不需要那些格式的話，寫 `none` 也可以。

### Docker

```sh
export GOLALINK_PASSWORD='a-long-random-password'
docker compose up -d
docker compose logs -f
```

Golalink 預設聽 `127.0.0.1:2333`。要接受遠端客戶端就設
`GOLALINK_ADDRESS=0.0.0.0:2333`，但對外開放前請先加上 TLS 或來源限制。

## 設定

`config.example.yaml` 列出每個選項，各有一行註解。複製一份再改你要的：

```sh
cp config.example.yaml config.yaml
```

```powershell
Copy-Item config.example.yaml config.yaml
```

沒給 `-config` 時，Golalink 會在工作目錄找 `config.yaml`、`config.yml` 或
`config.json`，所以解壓後的 release 直接跑就行。環境變數一律覆蓋檔案。欄位名稱
是嚴格比對的：打錯字會讓啟動失敗，不會被默默忽略。

`golalink -check` 會驗證設定與 media helper 後直接結束，不會開始監聽。

[設定](docs/configuration.md)文件記載了每個選項與環境變數。

## 連接機器人

用支援 v4.2 `channelId` 的客戶端，指向 Golalink：

```http
Authorization: your-node-password
User-Id: your-discord-bot-user-id
Client-Name: your-client/1.0.0
```

連上 `/v4/websocket`，讀出 `ready.sessionId`，然後載入曲目：

```http
GET /v4/loadtracks?identifier=ytsearch%3Anever%20gonna%20give%20you%20up
Authorization: your-node-password
```

從結果裡挑一筆，把它的 `encoded` 當成 `track.encoded`，連同由機器人語音狀態與
語音伺服器事件組出來的 `voice` 物件，送進
`PATCH /v4/sessions/{sessionId}/players/{guildId}`。

有兩個地方常讓人卡住。搜尋結果不能當成單一曲目的 `identifier`，要傳 `encoded`。
還有播放開始的時機是收到 `TrackStartEvent`，不是 REST 呼叫回傳的時候。

## 端點

| 端點 | 用途 |
|---|---|
| `GET /version` | 協定版本 |
| `GET /healthz` | 存活檢查，不需驗證 |
| `GET /v4/info` | 已啟用的來源、版本、濾鏡 |
| `GET /v4/stats` | Lavalink 形狀的統計 |
| `GET /v4/lyrics` | 指定 encoded 曲目的歌詞 |
| `GET /v4/sessions/{sessionId}/players/{guildId}/track/lyrics` | 播放器目前這首的歌詞 |
| `GET /metrics` | Prometheus 指標，需要節點密碼 |

`/v4/metrics` 跟 `/metrics` 提供同樣的指標。

`/healthz` 和 `/version` 刻意不驗證，讓容器編排系統能探測行程。其餘一律需要節點密碼。

## 專案結構

```text
cmd/golalink/       進入點
internal/config/    設定載入與驗證
internal/logging/   主控台與檔案日誌、啟動字樣
internal/tools/     media helper 的尋找與下載
internal/server/    HTTP、WebSocket、session、指標、歌詞路由
internal/player/    播放狀態、事件與取消
internal/source/    YouTube、SoundCloud、Spotify、Deezer、HTTP、yt-dlp
internal/audio/     FFmpeg 管線、濾鏡、Ogg 與 Opus 封裝
internal/audio/pure/        行程內解碼器、重採樣器與 Opus 編碼器
internal/audio/dispatch/    在兩個音訊引擎之間選擇
internal/lyrics/    歌詞來源與 LRC 解析
internal/voice/     DisGo 與 DAVE 傳輸
internal/protocol/  Lavalink JSON 型別與曲目編解碼
internal/netguard/  對外 URL、DNS 與 IP 限制
internal/system/    CPU 取樣
docs/               文件
```

## 建置

目標清單放在 `.github/workflows/build.yml`，`.github/workflows/ci.yml` 會在每次
變更時把整份清單編過一遍，所以不會等到推 tag 才發現某個目標壞掉。`release.yml`
把有 tag 的版本發布到 GitHub Releases，並推多架構映像到 `ghcr.io`；
`nightly.yml` 則是排程對預設分支的最新提交做一樣的事。

要在本機重現 release 的建置：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-s -w -X golalink/internal/server.Version=v0.1.0" \
  -o bin/golalink ./cmd/golalink
```

## 快速建置

[Linux](/build/build-linux.sh)

[MacOS](/build/build-macos.sh)

[Windows](/build/build-windows.bat)

## 文件

- [設定](docs/configuration.md) —— 每個選項與環境變數
- [來源](docs/sources.md) —— 各來源怎麼解析
- [相容性](docs/compatibility.md) —— Lavalink v4 涵蓋範圍與已知差異
- [第三方](docs/third-party.md) —— 相依套件與其授權

## 授權

GNU General Public License v3.0，全文在 [LICENSE](LICENSE)。
Copyright (C) 2026 Golalink contributors。

GPL 是強 copyleft 授權：如果你散布修改過的 Golalink，就必須把同樣的自由傳下去，
並把對應的原始碼提供給收到的人。原封不動地跑，或只給自己用，就沒有額外義務。
未修改的原始碼公開在 <https://github.com/Golalink/Golalink>。

第三方相依套件保有自己的授權，都與 GPL 相容；見[第三方](docs/third-party.md)。
