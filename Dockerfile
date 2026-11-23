# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api &&     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# Transcoding worker: the only image that carries ffmpeg.
FROM alpine:3.20 AS worker
RUN apk add --no-cache ca-certificates tzdata ffmpeg && adduser -D -u 10001 app
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
