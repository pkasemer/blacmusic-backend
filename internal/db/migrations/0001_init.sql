-- Core schema: users, auth sessions, and the unified catalog.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    display_name  text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  text NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens(user_id);

CREATE TABLE artists (
    id         text PRIMARY KEY DEFAULT 'art_' || replace(gen_random_uuid()::text, '-', ''),
    name       text NOT NULL,
    image_url  text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX artists_name_trgm ON artists USING gin (name gin_trgm_ops);

CREATE TABLE albums (
    id           text PRIMARY KEY DEFAULT 'alb_' || replace(gen_random_uuid()::text, '-', ''),
    title        text NOT NULL,
    artist_id    text REFERENCES artists(id) ON DELETE SET NULL,
    artwork_url  text,
    release_date date,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tracks (
    id          text PRIMARY KEY DEFAULT 'trk_' || replace(gen_random_uuid()::text, '-', ''),
    title       text NOT NULL,
    artist_id   text REFERENCES artists(id) ON DELETE SET NULL,
    album_id    text REFERENCES albums(id) ON DELETE SET NULL,
    duration_ms integer NOT NULL DEFAULT 0,
    artwork_url text,
    explicit    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tracks_title_trgm ON tracks USING gin (title gin_trgm_ops);

-- One row per place a track can be played from. Lower priority wins.
-- For source='native', media_key is the object key in S3-compatible storage.
-- For client-resolved sources (e.g. 'youtube'), source_id is the provider's id
-- and media_key stays NULL.
CREATE TABLE track_sources (
    track_id     text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    source       text NOT NULL,
    source_id    text NOT NULL,
    media_key    text,
    codec        text,
    bitrate_kbps integer,
    priority     integer NOT NULL DEFAULT 100,
    PRIMARY KEY (source, source_id)
);
CREATE INDEX track_sources_track_idx ON track_sources(track_id, priority);
