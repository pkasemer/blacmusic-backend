-- Songs and artists the team keeps out of what is suggested: mixes, autoplay and "Your sound". They stay playable and
-- searchable. Undoing a block deletes its row; who blocked and unblocked what, and why, stays in the audit log.
CREATE TABLE rec_blocks (
    id         bigserial PRIMARY KEY,
    track_id   text REFERENCES tracks(id) ON DELETE CASCADE,
    artist_id  text REFERENCES artists(id) ON DELETE CASCADE,
    reason     text NOT NULL,
    blocked_by uuid REFERENCES users(id) ON DELETE SET NULL,
    blocked_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((track_id IS NULL) <> (artist_id IS NULL))
);
CREATE UNIQUE INDEX rec_blocks_track_idx ON rec_blocks (track_id) WHERE track_id IS NOT NULL;
CREATE UNIQUE INDEX rec_blocks_artist_idx ON rec_blocks (artist_id) WHERE artist_id IS NOT NULL;

-- Every song that is blocked, itself or through its artist (songs the artist adds later too). Everything that
-- suggests songs leaves these out.
CREATE VIEW rec_blocked_tracks AS
    SELECT b.track_id FROM rec_blocks b WHERE b.track_id IS NOT NULL
    UNION
    SELECT t.id FROM rec_blocks b JOIN tracks t ON t.artist_id = b.artist_id;
