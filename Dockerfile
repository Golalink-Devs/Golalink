FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X golalink/internal/server.Version=${VERSION}" \
    -o /golalink ./cmd/golalink

# yt-dlp uses a JavaScript runtime for YouTube signature challenges. The native
# InnerTube client does not, so this only serves the yt-dlp fallback.
FROM node:22-bookworm-slim AS node

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home golalink \
    && mkdir -p /app/.tools \
    && chown -R golalink:golalink /app
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=build /golalink /usr/local/bin/golalink
# FFmpeg comes from the distribution and is pinned by path, because a bare name
# only ever means a file inside the tools directory. yt-dlp is downloaded on
# first start, which is why auto download is on here even though the default is
# off. XDG_CACHE_HOME points at tmpfs because the container filesystem is
# read-only.
ENV GOLALINK_ADDRESS="0.0.0.0:2333" \
    GOLALINK_FFMPEG="/usr/bin/ffmpeg" \
    GOLALINK_TOOLS_DIRECTORY="/app/.tools" \
    GOLALINK_AUTO_DOWNLOAD="true" \
    XDG_CACHE_HOME="/tmp"
WORKDIR /app
USER golalink
EXPOSE 2333
STOPSIGNAL SIGTERM
ENTRYPOINT ["golalink"]
