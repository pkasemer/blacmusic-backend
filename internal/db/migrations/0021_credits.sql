-- Who worked on a release or on one of its tracks (producers, writers, engineers, labels...). A credit
-- with no track belongs to the whole release. The artist replaces the whole list at once, so ids are not
-- stable across saves.
CREATE TABLE release_credits (
    id         text PRIMARY KEY DEFAULT 'crd_' || replace(gen_random_uuid()::text, '-', ''),
    release_id text NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    track_id   text REFERENCES tracks(id) ON DELETE CASCADE,
    name       text NOT NULL,
    role       text NOT NULL,
    role_label text NOT NULL DEFAULT '',
    artist_id  text REFERENCES artists(id) ON DELETE SET NULL,
    position   integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX release_credits_release_idx ON release_credits (release_id, position);
CREATE INDEX release_credits_track_idx ON release_credits (track_id) WHERE track_id IS NOT NULL;
