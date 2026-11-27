-- What mixes and autoplay were asked for and what was suggested, kept to build better recommendations later.
-- It goes with the account when the account is deleted (the privacy policy says so).
CREATE TABLE mix_requests (
    id             bigserial PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id       text NOT NULL, -- the song it started from
    reason         text NOT NULL CHECK (reason IN ('mix', 'autoplay')),
    suggested_ids  text[] NOT NULL DEFAULT '{}',
    at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mix_requests_user_idx ON mix_requests (user_id, at DESC);
