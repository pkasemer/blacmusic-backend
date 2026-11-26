-- Song lyrics, written by the artist: plain text, or timed lines (LRC) that the player follows.
-- text always holds the plain lyrics (for timed lyrics it is the lines joined); lines is the timed list
-- [{"timeMs":12340,"text":"..."}] and is empty for plain lyrics.
CREATE TABLE track_lyrics (
    track_id   text PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    synced     boolean NOT NULL DEFAULT false,
    text       text NOT NULL,
    lines      jsonb NOT NULL DEFAULT '[]',
    updated_at timestamptz NOT NULL DEFAULT now()
);
