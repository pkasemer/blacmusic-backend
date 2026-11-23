-- Playlists and the user's library (liked tracks, saved albums, followed artists).

CREATE TABLE playlists (
    id          text PRIMARY KEY DEFAULT 'pl_' || replace(gen_random_uuid()::text, '-', ''),
    owner_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    is_public   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX playlists_owner_idx ON playlists(owner_id, updated_at DESC);

-- position is a contiguous 0-based index per playlist, maintained by the API
-- inside a transaction that locks the playlist row. The same track may appear
-- more than once.
CREATE TABLE playlist_items (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    playlist_id text NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    track_id    text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    position    integer NOT NULL,
    added_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    added_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX playlist_items_pos_idx ON playlist_items(playlist_id, position);

CREATE TABLE library_tracks (
    user_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, track_id)
);
CREATE INDEX library_tracks_recent_idx ON library_tracks(user_id, added_at DESC);

CREATE TABLE library_albums (
    user_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    album_id text NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, album_id)
);
CREATE INDEX library_albums_recent_idx ON library_albums(user_id, added_at DESC);

CREATE TABLE library_artists (
    user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    artist_id text NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    added_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, artist_id)
);
CREATE INDEX library_artists_recent_idx ON library_artists(user_id, added_at DESC);
