-- Bulk catalogue import: limited keys for scripts, and import jobs that survive restarts.

-- A key can only create and follow import jobs. Only its hash is stored; the key itself is shown once.
CREATE TABLE api_keys (
    id           text PRIMARY KEY DEFAULT 'key_' || replace(gen_random_uuid()::text, '-', ''),
    name         text NOT NULL,
    prefix       text NOT NULL,                -- the first characters, to tell keys apart
    token_hash   text NOT NULL UNIQUE,
    created_by   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    last_used_at timestamptz
);

CREATE TABLE import_jobs (
    id               text PRIMARY KEY DEFAULT 'imp_' || replace(gen_random_uuid()::text, '-', ''),
    created_by       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    api_key_id       text REFERENCES api_keys(id) ON DELETE SET NULL,
    idempotency_key  text NOT NULL,
    artist_id        text REFERENCES artists(id) ON DELETE SET NULL,
    artist_name      text NOT NULL,
    artist_bio       text NOT NULL DEFAULT '',
    picture_file     text NOT NULL DEFAULT '',
    release_type     text NOT NULL CHECK (release_type IN ('single', 'album')),
    publish          text NOT NULL CHECK (publish IN ('public', 'draft')),
    album_title      text NOT NULL DEFAULT '',
    album_genre      text NOT NULL DEFAULT '',
    album_cover_file text NOT NULL DEFAULT '',
    album_date       text NOT NULL DEFAULT '',
    album_release_id text NOT NULL DEFAULT '',
    rights_basis     text NOT NULL,
    status           text NOT NULL DEFAULT 'awaiting_files'
                     CHECK (status IN ('awaiting_files', 'processing', 'completed', 'completed_with_errors', 'failed', 'cancelled')),
    error            text NOT NULL DEFAULT '',
    warnings         text[] NOT NULL DEFAULT '{}',   -- things that did not stop the import (a picture that could not be used)
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    UNIQUE (created_by, idempotency_key)
);
CREATE INDEX import_jobs_status_idx ON import_jobs (status, created_at);

-- The files the caller sends straight to storage. name is the manifest name (only a label: the object is stored
-- under storage_key).
CREATE TABLE import_files (
    job_id       text NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    name         text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('audio', 'image')),
    size_bytes   bigint NOT NULL,
    storage_key  text NOT NULL,
    multipart_id text NOT NULL DEFAULT '',
    part_size    bigint NOT NULL DEFAULT 0,
    actual_bytes bigint NOT NULL DEFAULT 0,
    image_id     text NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, name)
);

CREATE TABLE import_items (
    job_id        text NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    position      integer NOT NULL,
    title         text NOT NULL,
    genre         text NOT NULL DEFAULT '',
    explicit      boolean NOT NULL DEFAULT false,
    release_date  text NOT NULL DEFAULT '',
    audio_file    text NOT NULL,
    cover_file    text NOT NULL DEFAULT '',
    note          text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'created', 'skippedDuplicate', 'failed')),
    attempts      integer NOT NULL DEFAULT 0,
    release_id    text NOT NULL DEFAULT '',
    upload_id     text NOT NULL DEFAULT '',
    track_id      text NOT NULL DEFAULT '',
    error_code    text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, position)
);
