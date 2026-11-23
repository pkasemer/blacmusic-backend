-- Podcasts: shows ingested from RSS feeds, their episodes, subscriptions and
-- per-user listening progress. Audio stays on the publisher's host; we only
-- store and serve the enclosure URL.

CREATE TABLE shows (
    id              text PRIMARY KEY DEFAULT 'shw_' || replace(gen_random_uuid()::text, '-', ''),
    feed_url        text NOT NULL UNIQUE,
    title           text NOT NULL,
    author          text NOT NULL DEFAULT '',
    description     text NOT NULL DEFAULT '',
    image_url       text NOT NULL DEFAULT '',
    website_url     text NOT NULL DEFAULT '',
    language        text NOT NULL DEFAULT '',
    explicit        boolean NOT NULL DEFAULT false,
    -- conditional GET validators and refresh bookkeeping
    etag            text NOT NULL DEFAULT '',
    last_modified   text NOT NULL DEFAULT '',
    last_fetched_at timestamptz,
    next_fetch_at   timestamptz NOT NULL DEFAULT now(),
    fail_count      integer NOT NULL DEFAULT 0,
    fetch_error     text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX shows_title_trgm ON shows USING gin (title gin_trgm_ops);
CREATE INDEX shows_due_idx ON shows (next_fetch_at);

CREATE TABLE episodes (
    id             text PRIMARY KEY DEFAULT 'epi_' || replace(gen_random_uuid()::text, '-', ''),
    show_id        text NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    guid           text NOT NULL,
    title          text NOT NULL,
    description    text NOT NULL DEFAULT '',
    audio_url      text NOT NULL,
    mime_type      text NOT NULL DEFAULT '',
    size_bytes     bigint NOT NULL DEFAULT 0,
    duration_ms    integer NOT NULL DEFAULT 0,
    published_at   timestamptz NOT NULL,
    season         integer,
    episode_number integer,
    episode_type   text NOT NULL DEFAULT 'full',
    image_url      text NOT NULL DEFAULT '',
    explicit       boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (show_id, guid)
);
CREATE INDEX episodes_show_published_idx ON episodes (show_id, published_at DESC, id);

CREATE TABLE show_subscriptions (
    user_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    show_id  text NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, show_id)
);
CREATE INDEX show_subscriptions_show_idx ON show_subscriptions (show_id);

CREATE TABLE episode_progress (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    episode_id  text NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
    position_ms integer NOT NULL DEFAULT 0,
    completed   boolean NOT NULL DEFAULT false,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, episode_id)
);
CREATE INDEX episode_progress_recent_idx ON episode_progress (user_id, updated_at DESC) WHERE NOT completed;
