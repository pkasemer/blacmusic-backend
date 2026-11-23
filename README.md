# Blac Music Backend

Go API for the Blac Music streaming app (music + podcasts) with **pluggable audio sources**.
Stack: Go 1.23, chi, PostgreSQL 17, Redis 7, S3-compatible storage (RustFS locally; MinIO's public images were withdrawn, the code only uses the generic S3 protocol), ffmpeg for transcoding, Docker.
Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Run everything (Docker)

```powershell
docker compose up --build
```

API on http://localhost:8080, storage console on http://localhost:9001 (`blacminio` / `blacminio-secret`).

```powershell
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

## Vet and test without installing Go

```powershell
docker run --rm -v "${PWD}:/src" -w /src golang:1.23-alpine sh -c "go mod tidy && gofmt -l . && go vet ./... && go test ./..."
```

## Try the API

```powershell
# register (returns accessToken + refreshToken)
curl -X POST http://localhost:8080/v1/auth/register -H "Content-Type: application/json" `
  -d '{\"email\":\"me@example.com\",\"password\":\"supersecret1\",\"displayName\":\"Me\"}'

# seed a demo track, upload an mp3 to blac-media at demo/sample.mp3 via the storage console (http://localhost:9001)
Get-Content scripts/seed.sql | docker compose exec -T postgres psql -U blac -d blac

# then, with the access token
curl http://localhost:8080/v1/search?q=demo -H "Authorization: Bearer <accessToken>"
curl http://localhost:8080/v1/playback/trk_demo -H "Authorization: Bearer <accessToken>"
```

## Uploading audio

Only users with role `creator` or `admin` can upload. Promote an account (operator action, no endpoint yet):

```powershell
docker compose exec -T postgres psql -U blac -d blac -c "UPDATE users SET role='creator' WHERE email='you@example.com'"
```

Flow (details in the shared notes, `backend-api-uploads-quality.md`):

1. `POST /v1/uploads` with title, artistName, filename, sizeBytes returns a presigned **PUT** URL.
2. The client PUTs the file straight to object storage (it never passes through the API).
3. `POST /v1/uploads/{id}/complete` queues it.
4. The `worker` container probes the file and transcodes it once into three AAC-LC `.m4a` renditions, then creates the track:

| quality | bitrate | per hour | use |
|---|---|---|---|
| `low` | 48 kbps | ~21 MB | low-data mode |
| `medium` | 128 kbps | ~58 MB | standard listening (default) |
| `high` | 256 kbps | ~115 MB | best quality |

5. `GET /v1/playback/{trackId}?quality=low|medium|high` returns a presigned URL for that rendition (default medium) plus the list of available qualities with sizes. Originals are kept privately under `originals/` and never served.

## Layout

```
cmd/api/              entrypoint, wires dependencies and sources
internal/config/      env config
internal/db/          pgx pool + embedded SQL migrations (internal/db/migrations)
internal/auth/        register / login / refresh rotation / JWT middleware
internal/source/      Provider interface, PlaybackDescriptor, registry
internal/source/native/  first-party catalog in S3-compatible storage
internal/catalog/     search + track metadata
internal/playback/    resolves a track to a PlaybackDescriptor with source fallback
internal/storage/     S3 client, presigned GET/PUT, stat, download, upload
internal/media/       quality ladder, ffprobe validation, ffmpeg transcoding
internal/upload/      upload API (creator role) and the processing queue in Postgres
internal/worker/      claims queued uploads and runs probe -> transcode -> store -> create track
cmd/worker/           the transcoding worker (the only image with ffmpeg)
internal/ratelimit/   Redis fixed-window limiter (auth endpoints)
```

## Adding a source

Implement `source.Provider`, register it in `cmd/api/main.go`, and add `track_sources` rows with that
`source` name. A client-resolved source returns `Mode: client_resolve` and the app resolves it on-device.

## Implemented so far

Auth (register, login, refresh, logout, me), search, track lookup, playback resolve, readiness checks,
playlists (CRUD, ordered items, add/remove/move, private or public), library (liked tracks, saved albums,
followed artists, bulk `contains` check), creator uploads with low/medium/high transcoding and
quality-aware playback.
Not yet: podcasts, listen-together, admin UI, album/artist detail endpoints, cover art, upload cleanup of abandoned files.

## Tests

Unit tests run anywhere. The playlist, upload and worker tests need Postgres and skip without `TEST_DATABASE_URL`;
the worker tests also need ffmpeg. Use a separate database so tests never touch your dev data (and the running
worker never picks up test uploads):

```powershell
docker compose exec -T postgres psql -U blac -d blac -c "CREATE DATABASE blac_test"
docker run --rm --network backend_default -e TEST_DATABASE_URL="postgres://blac:blac@postgres:5432/blac_test?sslmode=disable" -v "${PWD}:/src" -w /src golang:1.23-alpine sh -c "apk add --no-cache ffmpeg && go test ./..."
```
