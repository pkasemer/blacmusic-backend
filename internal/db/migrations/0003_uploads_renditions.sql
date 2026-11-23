-- Uploads and transcoded renditions.
--
-- A creator uploads one original file. A worker turns it into three AAC
-- renditions (low / medium / high) and only then creates the track.

ALTER TABLE users
    ADD COLUMN role text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'creator', 'admin'));

-- One playable file per quality for a native track.
CREATE TABLE track_renditions (
    track_id     text   NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    quality      text   NOT NULL CHECK (quality IN ('low', 'medium', 'high')),
    media_key    text   NOT NULL,
    codec        text   NOT NULL,
    container    text   NOT NULL,
    bitrate_kbps integer NOT NULL,
    size_bytes   bigint NOT NULL,
    PRIMARY KEY (track_id, quality)
);

-- The upload is also the processing queue (status = 'queued' rows are claimed
-- with FOR UPDATE SKIP LOCKED).
--
-- track_id is reserved up front so object keys are stable across retries; the
-- tracks row only exists once processing succeeds.
CREATE TABLE uploads (
    id           text PRIMARY KEY DEFAULT 'upl_' || replace(gen_random_uuid()::text, '-', ''),
    user_id      uuid   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id     text   NOT NULL UNIQUE DEFAULT 'trk_' || replace(gen_random_uuid()::text, '-', ''),
    title        text   NOT NULL,
    artist_name  text   NOT NULL,
    album_title  text   NOT NULL DEFAULT '',
    explicit     boolean NOT NULL DEFAULT false,
    filename     text   NOT NULL,
    size_bytes   bigint NOT NULL,
    original_key text   NOT NULL,
    status       text   NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'queued', 'processing', 'ready', 'failed')),
    attempts     integer NOT NULL DEFAULT 0,
    error        text,
    locked_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX uploads_queue_idx ON uploads (created_at) WHERE status IN ('queued', 'processing');
CREATE INDEX uploads_user_idx  ON uploads (user_id, created_at DESC);
