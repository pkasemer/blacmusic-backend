# Blac Music Backend: Architecture Draft

Status: DRAFT for user review. No code has been written and the stack is not final.
Decision made: **pluggable audio sources** (legit catalog first, YouTube optional and client-side only).

## 1. Principles

1. The backend owns **metadata, accounts, library, sync and social features**. It does not have to own the audio.
2. Every track/episode has a `source` and a `sourceId`. The backend resolves *how to play it* through a **Source Provider** interface.
3. Sources are plugins. Adding or removing one never changes the public API or the mobile app's player contract.
4. The backend never scrapes or proxies YouTube. If YouTube is enabled it is a **client-side source plugin in the mobile app**; the backend only stores references (`source=youtube`, `sourceId=<videoId>`) for the user's library/playlists.

## 2. Source Provider interface

Each provider implements:

| Method | Purpose |
|---|---|
| `search(query, type, page)` | Search tracks/albums/artists/podcasts in that source |
| `getTrack(id)` / `getAlbum(id)` / `getArtist(id)` / `getShow(id)` / `getEpisode(id)` | Normalised metadata |
| `resolvePlayback(id, ctx)` | Returns a **PlaybackDescriptor** (below) |
| `capabilities` | e.g. `search`, `lossless`, `offline`, `lyrics`, `clientResolved` |

**PlaybackDescriptor** (what the app receives):

```jsonc
{
  "mode": "url",                 // "url" | "hls" | "dash" | "client_resolve"
  "url": "https://cdn.../master.m3u8",
  "expiresAt": "2026-11-23T12:00:00Z",
  "format": { "codec": "aac", "bitrateKbps": 256 },
  "headers": {},                 // optional, if the CDN needs them
  "clientResolve": null          // for mode=client_resolve: { "provider": "youtube", "id": "..." }
}
```

`client_resolve` tells the mobile app to resolve the stream itself with an on-device plugin. This is how the YouTube option fits without the server touching YouTube.

## 3. Planned sources (priority order)

| # | Source | Content | Notes |
|---|---|---|---|
| 1 | `native` | Our own uploads / licensed catalog | Stored in S3-compatible storage, transcoded by the ffmpeg worker into three progressive AAC-LC `.m4a` renditions (low 48 / medium 128 / high 256 kbps). Needs rights agreements or artist uploads |
| 2 | `podcast` | Podcasts | RSS feeds + Podcast Index API for discovery. Legal, free; audio is the publisher's enclosure URL |
| 3 | `audius`, `jamendo` | Independent / CC music | Public APIs, check each licence and attribution rule |
| 4 | `youtube` (optional) | YouTube Music | **Client-resolved only**, off by default, ToS and store-policy risk accepted by the product owner. Backend stores references only |

## 4. Backend modules

- **auth**: signup/login, JWT access + refresh, social login later, device sessions
- **catalog**: unified Track/Album/Artist/Show/Episode model with source mapping; search aggregation across providers
- **playback**: `resolvePlayback`, signed/expiring URLs, play-event ingestion
- **library**: likes, saved albums/shows, follows, download manifest
- **playlists**: CRUD, collaborative playlists, reorder
- **podcasts**: feed ingestion worker, episode progress (resume position), new-episode notifications
- **history & recs**: listening history, simple recommendations (later: ML)
- **sync**: multi-device state (queue, now-playing), optional **listen-together** via WebSocket
- **media pipeline**: upload, ffmpeg transcode, HLS packaging, artwork
- **admin**: content moderation, source health dashboard

## 5. API surface (v1, REST + WebSocket)

```
POST /v1/auth/register | /login | /refresh | /logout
GET  /v1/search?q=&type=&sources=
GET  /v1/tracks/{id}   GET /v1/albums/{id}   GET /v1/artists/{id}
GET  /v1/shows/{id}    GET /v1/episodes/{id}
GET  /v1/playback/{trackOrEpisodeId}            -> PlaybackDescriptor
POST /v1/playback/events                        (start, progress, complete)
GET/POST/DELETE /v1/me/library/*
GET/POST/PATCH/DELETE /v1/playlists, /v1/playlists/{id}/items
GET/PUT /v1/me/podcasts/subscriptions, /v1/episodes/{id}/progress
GET  /v1/home                                    (personalised feed)
WS   /v1/ws                                      (sync, listen-together)
```

IDs are Blac Music IDs (`trk_...`, `epi_...`); each maps to one or more `(source, sourceId)` rows so the same song can fall back between sources.

## 6. Data model (core tables)

`users`, `devices`, `artists`, `albums`, `tracks`, `shows`, `episodes`, `track_sources(track_id, source, source_id, quality)`, `playlists`, `playlist_items`, `library_items`, `follows`, `play_history`, `episode_progress`, `uploads`, `media_assets`.

## 7. Stack (LOCKED by the user)

- **Language:** Go 1.23 with `chi` router, `pgx` and `log/slog`
- **DB:** PostgreSQL 17 (`pg_trgm` for search; Meilisearch later if needed)
- **Cache/queues:** Redis 7 (rate limiting now; transcode jobs currently queue in Postgres (`uploads` table, `FOR UPDATE SKIP LOCKED`); Redis queue for feed ingestion later, e.g. asynq)
- **Storage/CDN:** S3-compatible (RustFS locally; Cloudflare R2 or S3 in prod) + CDN, HLS delivery later
- **Realtime:** WebSocket gateway (gorilla/websocket or coder/websocket), later
- **Infra:** Docker + Docker Compose, GitHub Actions
- **Docs:** OpenAPI to be written/generated and shared with the mobile session

Scaffold status: see README.md ("Implemented so far").

## 8. Open questions for the user

1. Mobile stack (native Kotlin/Swift, Flutter, React Native)? It drives the backend language choice.
2. Where will the first music content come from (own uploads, licensed deals, open catalogs only)?
3. Is the optional YouTube client-side plugin wanted in v1, or deferred?
4. Monetisation/tiers (free vs subscription), which affects entitlements in `playback`.
5. Hosting preference and budget.
6. Features required in v1 (listen-together, lyrics, offline downloads, recommendations)?
