-- YouTube connect (read-only metadata) and queued transfers of an artist's
-- songs into releases. The audio always comes from the artist's own uploads;
-- nothing is ever fetched from YouTube except metadata and thumbnails.

-- One connection per user. The refresh token is stored encrypted (AES-256-GCM).
CREATE TABLE youtube_connections (
    user_id           uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    channel_id        text NOT NULL,
    channel_title     text NOT NULL,
    thumbnail_url     text NOT NULL DEFAULT '',
    refresh_token_enc bytea NOT NULL,
    scopes            text NOT NULL,
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'needs_reconnect')),
    connected_at      timestamptz NOT NULL DEFAULT now(),
    last_synced_at    timestamptz
);

-- A started authorisation: single use, ten minutes. The PKCE verifier lives here, never in the URL.
CREATE TABLE youtube_oauth_states (
    state      text PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    verifier   text NOT NULL,
    expires_at timestamptz NOT NULL
);

-- Cache of the channel's videos (API data: refreshed on demand, purged on disconnect).
CREATE TABLE youtube_videos (
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    video_id     text NOT NULL,
    title        text NOT NULL,
    description  text NOT NULL DEFAULT '',
    published_at timestamptz NOT NULL,
    duration_sec integer NOT NULL DEFAULT 0,
    thumbnail_url text NOT NULL DEFAULT '',
    fetched_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, video_id)
);

-- A transfer: a set of songs being turned into releases.
--   collecting -> queued -> in_progress -> completed | failed   (retrying between attempts, cancelled any time)
CREATE TABLE transfers (
    id              text PRIMARY KEY DEFAULT 'tr_' || replace(gen_random_uuid()::text, '-', ''),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    artist_id       text NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    status          text NOT NULL DEFAULT 'collecting'
                    CHECK (status IN ('collecting', 'queued', 'in_progress', 'retrying', 'completed', 'failed', 'cancelled')),
    idempotency_key text,
    attempt         integer NOT NULL DEFAULT 0,
    max_attempts    integer NOT NULL DEFAULT 5,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until     timestamptz,
    worker_id       text,
    error           text NOT NULL DEFAULT '',
    reason          text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    submitted_at    timestamptz,
    started_at      timestamptz,
    finished_at     timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX transfers_idem_idx ON transfers (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
-- At most one open transfer per artist.
CREATE UNIQUE INDEX transfers_one_open_idx ON transfers (artist_id)
    WHERE status IN ('collecting', 'queued', 'in_progress', 'retrying');
CREATE INDEX transfers_queue_idx ON transfers (submitted_at) WHERE status IN ('queued', 'retrying');
CREATE INDEX transfers_user_idx ON transfers (user_id, created_at DESC);

CREATE TABLE transfer_items (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    transfer_id     text NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
    position        integer NOT NULL,
    video_id        text NOT NULL,
    title           text NOT NULL,
    release_type    text NOT NULL CHECK (release_type IN ('single', 'album')),
    album_title     text NOT NULL DEFAULT '',
    genre           text NOT NULL DEFAULT '',
    release_date    date,
    explicit        boolean NOT NULL DEFAULT false,
    track_number    integer,
    thumbnail_url   text NOT NULL DEFAULT '',
    release_id      text REFERENCES albums(id) ON DELETE SET NULL,
    upload_id       text REFERENCES uploads(id) ON DELETE SET NULL,
    track_id        text,
    cover_image_id  text,
    cover_done      boolean NOT NULL DEFAULT false,
    status          text NOT NULL DEFAULT 'awaiting_audio'
                    CHECK (status IN ('awaiting_audio', 'uploading', 'processing', 'cover', 'publishing', 'done', 'failed', 'skipped_duplicate', 'dropped')),
    error           text NOT NULL DEFAULT '',
    duplicate_of    text NOT NULL DEFAULT '',
    UNIQUE (transfer_id, video_id)
);
CREATE INDEX transfer_items_upload_idx ON transfer_items (upload_id) WHERE upload_id IS NOT NULL;
