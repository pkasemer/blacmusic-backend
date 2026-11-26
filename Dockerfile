# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api &&     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# Transcoding worker: the only image that carries ffmpeg.
FROM alpine:3.20 AS worker
# yt-dlp and a JavaScript runtime are only added when the image is built with --build-arg WITH_YTDLP=1.
# They serve the parked "import audio from the artist's own YouTube videos" feature (YOUTUBE_AUDIO_FETCH),
# which is off in production, and they add about 150 MB. YouTube changes often: rebuild to get the latest
# yt-dlp (or pin one with --build-arg YTDLP_VERSION=...).
ARG WITH_YTDLP=0
ARG YTDLP_VERSION=
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
 && if [ "$WITH_YTDLP" = "1" ]; then \
      apk add --no-cache python3 py3-pip nodejs \
      && python3 -m venv /opt/ytdlp \
      && /opt/ytdlp/bin/pip install --no-cache-dir "yt-dlp${YTDLP_VERSION:+==$YTDLP_VERSION}" \
      && ln -s /opt/ytdlp/bin/yt-dlp /usr/local/bin/yt-dlp; \
    fi \
 && adduser -D -u 10001 app
USER app
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["/usr/local/bin/worker"]

# API (last stage, so a plain `docker build .` produces the API image).
FROM alpine:3.20 AS api
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
USER app
COPY --from=build /out/api /usr/local/bin/api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
