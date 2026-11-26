-- Public playlists can be discovered, featured by the Blac team, saved by other
-- users and given a cover picture.

ALTER TABLE playlists
    ADD COLUMN is_featured    boolean NOT NULL DEFAULT false,
    ADD COLUMN featured_at    timestamptz,
    ADD COLUMN cover_image_id text;

-- Playlists other users saved to their library (the "popular" signal).
CREATE TABLE library_playlists (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    playlist_id text NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, playlist_id)
);
CREATE INDEX library_playlists_recent_idx ON library_playlists(user_id, added_at DESC);
CREATE INDEX library_playlists_playlist_idx ON library_playlists(playlist_id);

-- Browsing walks public playlists newest first.
CREATE INDEX playlists_public_idx ON playlists(updated_at DESC) WHERE is_public;
