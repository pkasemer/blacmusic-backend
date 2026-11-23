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

## Artist mode and uploads

Anyone can become an artist: verify your email (a 6-digit code), create an artist profile (10 GB free storage),
then upload. Full contract: `backend-api-artist-mode.md` in the notes folder.

1. `POST /v1/auth/email/send-code`, `POST /v1/auth/email/verify {code}`. Without `SMTP_HOST` (development) the code is
   written to the API log: `docker compose logs api`.
2. `POST /v1/artists`, then optionally an avatar and a cover (`POST /v1/images`, PUT the file, `.../complete`; images are
   validated and re-encoded, EXIF dropped, served publicly from `/v1/media/...`).
3. `POST /v1/releases` (single or album, starts as a **draft**).
4. `POST /v1/uploads` (confirm you own the rights) returns a presigned **PUT** URL; the file goes straight to object
   storage; `POST /v1/uploads/{id}/complete` queues it. Quota is reserved up front (402 `quota_exceeded` when it does not fit).
5. The `worker` container probes the file and transcodes it once into three AAC-LC `.m4a` renditions, then creates the
   track inside the release:

| quality | bitrate | per hour | use |
|---|---|---|---|
| `low` | 48 kbps | ~21 MB | low-data mode |
| `medium` | 128 kbps | ~58 MB | standard listening (default) |
| `high` | 256 kbps | ~115 MB | best quality |

6. `POST /v1/releases/{id}/publish` (now, or with `publishAt` to schedule) makes the release and its tracks public.
   Drafts are visible to the owning artist only (search, lookup, playback, library, playlists).
7. `GET /v1/playback/{trackId}?quality=low|medium|high` returns a presigned URL for that rendition (default medium) plus
   the available qualities with sizes. Originals are kept privately under `originals/` and never served.

Storage is counted from the original files only. The verified badge is requested with
`POST /v1/artists/{id}/verification-request` and decided by an admin (`/v1/admin/...`). Make an admin with SQL:

```powershell
docker compose exec -T postgres psql -U blac -d blac -c "UPDATE users SET role='admin' WHERE email='you@example.com'"
```

Real email: set `SMTP_HOST`, `SMTP_PORT` (587, STARTTLS), `SMTP_USER`, `SMTP_PASSWORD`, `SMTP_FROM`. Any SMTP provider works
(Amazon SES, Mailgun, Resend, ...). The API refuses to start outside development without `SMTP_HOST`.
`PUBLIC_API_URL` is the address apps use to reach the API (used in image links); for the Android emulator set it to
`http://10.0.2.2:8080`.

## Podcasts

Podcasts come from public RSS feeds. The audio stays on the publisher's server; the API stores show and episode
metadata and hands the app the publisher's URL at playback time. Full contract: `backend-api-podcasts.md` in the notes folder.

```
POST /v1/shows {feedUrl}                         add a feed by URL (also subscribes you; 201 new, 200 known)
GET  /v1/shows/search?q=   /v1/shows/{id}   /v1/shows/{id}/episodes?order=newest|oldest&limit=&offset=
GET  /v1/episodes/{id}     PUT /v1/episodes/{id}/progress {positionMs, durationMs?, completed?}
GET  /v1/me/shows          PUT|DELETE /v1/me/shows/{id}      GET /v1/me/episodes/in-progress
GET  /v1/playback/{epi_...}                      -> the enclosure URL
```

The worker container also refreshes every followed show (default hourly, with conditional GET and exponential
backoff on failure); shows nobody follows are left alone.

Users choose feed URLs, so fetching is locked down: only http(s), no credentials in URLs, and the connection itself
refuses loopback, private, link-local (cloud metadata) and other internal addresses, including after redirects and
DNS changes; 10 MB and 15 s limits; feed text is reduced to plain text. `FEED_ALLOW_PRIVATE=true` switches the address
guard off for local development (docker-compose sets it so feeds can be served from your machine); the API refuses to
start with it in production.

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
internal/artist/      artist profiles, verification requests, storage quota ledger
internal/release/     releases (draft/scheduled/public), track edit and delete
internal/image/       avatar and cover upload, validation, re-encoding, public serving
internal/mailer/      verification emails (SMTP, or logged in development)
internal/admin/       review queue and manual storage grants
internal/upload/      upload API (artist, quota, rights) and the processing queue in Postgres
internal/worker/      claims queued uploads (probe -> transcode -> store -> create track) and runs maintenance
                      (scheduled releases go live, abandoned uploads are cleaned up)
internal/podcast/     RSS parsing, SSRF-safe fetching, shows/episodes/subscriptions/progress, background refresher
internal/source/podcast/  playback provider for episodes
cmd/worker/           the transcoding worker and feed refresher (the only image with ffmpeg)
internal/ratelimit/   Redis fixed-window limiter (auth endpoints)
```

## Adding a source

Implement `source.Provider`, register it in `cmd/api/main.go`, and add `track_sources` rows with that
`source` name. A client-resolved source returns `Mode: client_resolve` and the app resolves it on-device.

## Implemented so far

Auth (register, login, refresh, logout, me), search, track lookup, playback resolve, readiness checks,
playlists (CRUD, ordered items, add/remove/move, private or public), library (liked tracks, saved albums,
followed artists, bulk `contains` check), creator uploads with low/medium/high transcoding and
quality-aware playback, podcasts (RSS ingest, subscriptions, resume progress, background refresh), email verification,
artist profiles with verification requests, releases with draft/schedule/publish, images, storage quota, admin review.
Not yet: resumable multipart upload, loudness, play events and stats, offline downloads, reports and takedowns,
web billing, podcast discovery directory, new-episode notifications, listen-together, admin UI, album/artist detail endpoints, cover art, upload cleanup of abandoned files.

## Tests

Unit tests run anywhere. The playlist, upload and worker tests need Postgres and skip without `TEST_DATABASE_URL`;
the worker tests also need ffmpeg. Use a separate database so tests never touch your dev data (and the running
worker never picks up test uploads):

```powershell
docker compose exec -T postgres psql -U blac -d blac -c "CREATE DATABASE blac_test"
docker run --rm --network backend_default -e TEST_DATABASE_URL="postgres://blac:blac@postgres:5432/blac_test?sslmode=disable" -v "${PWD}:/src" -w /src golang:1.23-alpine sh -c "apk add --no-cache ffmpeg && go test ./..."
```
