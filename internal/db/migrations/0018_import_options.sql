-- The ways an artist can bring songs in. The backend decides which are on, so an option can be
-- opened (or closed) without releasing a new app. The app has its own wording for known ids and
-- uses title/description/note when they are set.
CREATE TABLE import_options (
    id          text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9_]{0,29}$'),
    enabled     boolean NOT NULL DEFAULT false,
    title       text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    note        text NOT NULL DEFAULT '',
    position    integer NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO import_options (id, enabled, note, position) VALUES
    ('manual', true, '', 1),
    ('youtube', false, 'Coming soon', 2);
