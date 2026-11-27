-- An artist's new or changed profile (name, bio, picture) waits for the team too. A new profile is not shown to
-- listeners until approved; the ones there already, and any the team makes, count as approved.
ALTER TABLE artists ADD COLUMN approved_at timestamptz DEFAULT now();
UPDATE artists SET approved_at = created_at;

-- The artist's picture is their account's, which everything listeners see reads. A new picture waiting for review is
-- kept here: the account shows it to its owner, everyone else keeps seeing avatar_image_id.
ALTER TABLE users ADD COLUMN next_avatar_image_id text;

CREATE TABLE artist_reviews (
    id           text PRIMARY KEY DEFAULT 'arv_' || replace(gen_random_uuid()::text, '-', ''),
    seq          bigserial,
    artist_id    text NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('new', 'update')),
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'withdrawn')),
    revision     integer NOT NULL DEFAULT 1,
    changes      jsonb NOT NULL DEFAULT '{}',
    submitted_at timestamptz NOT NULL DEFAULT now(),
    decided_at   timestamptz,
    decided_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    reason       text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX artist_reviews_one_pending ON artist_reviews (artist_id) WHERE status = 'pending';
CREATE INDEX artist_reviews_artist_idx ON artist_reviews (artist_id, seq DESC);
CREATE INDEX artist_reviews_queue_idx ON artist_reviews (submitted_at) WHERE status = 'pending';
