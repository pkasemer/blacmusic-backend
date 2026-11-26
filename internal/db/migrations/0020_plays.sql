-- Listening history that artists see as numbers (plays, listeners, where plays came from). One row per
-- counted play: the app reports a track once it has played about 30 seconds. The user is kept only as
-- long as the account exists (deleting the account makes the row anonymous but keeps the count).
-- track, release and artist are copied here so the numbers survive the track or release being deleted.
CREATE TABLE plays (
    id           bigserial PRIMARY KEY,
    user_id      uuid REFERENCES users(id) ON DELETE SET NULL,
    track_id     text NOT NULL,
    release_id   text,
    artist_id    text,
    listened_sec integer NOT NULL,
    completed    boolean NOT NULL DEFAULT false,
    source       text NOT NULL DEFAULT 'other',
    at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX plays_artist_idx ON plays (artist_id, at);
CREATE INDEX plays_release_idx ON plays (release_id, at);
CREATE INDEX plays_track_idx ON plays (track_id, at);
CREATE INDEX plays_user_track_idx ON plays (user_id, track_id, at DESC);
