-- How a person listens, as their app reports it: what they finish, skip, replay, like and add to playlists. It is
-- separate from plays (the counts artists see) and goes when the account is deleted.
CREATE TABLE listening_events (
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_id     text NOT NULL, -- made by the app: sending one again changes nothing
    type         text NOT NULL CHECK (type IN ('listen', 'like', 'unlike', 'playlist_add', 'session_start', 'session_end')),
    at           timestamptz NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    track_id     text NOT NULL DEFAULT '',
    session_id   text NOT NULL DEFAULT '',
    playlist_id  text NOT NULL DEFAULT '',
    listened_sec integer NOT NULL DEFAULT 0,
    duration_sec integer NOT NULL DEFAULT 0,
    completion   integer NOT NULL DEFAULT 0,
    outcome      text NOT NULL DEFAULT '' CHECK (outcome IN ('', 'completed', 'skipped', 'stopped')),
    replay       boolean NOT NULL DEFAULT false,
    source       text NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, event_id)
);
CREATE INDEX listening_events_user_idx ON listening_events (user_id, at DESC);

-- The "Your sound" playlist the server keeps for a listener. Deleting the playlist removes this row, and the next
-- time the app asks, a new one is made.
CREATE TABLE your_sound (
    user_id      uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    playlist_id  text NOT NULL UNIQUE REFERENCES playlists(id) ON DELETE CASCADE,
    refreshed_at timestamptz
);
-- songs the server has put in it, so that one the listener takes out does not come straight back
CREATE TABLE your_sound_offers (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id   text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    offered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, track_id)
);
-- true only for what the server added: it never removes anything else
ALTER TABLE playlist_items ADD COLUMN by_server boolean NOT NULL DEFAULT false;
