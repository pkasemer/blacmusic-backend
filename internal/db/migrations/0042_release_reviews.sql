-- What artists publish or change waits here for the team. A new release stays a draft until approved; a change to a
-- release that is online is held in `changes` while listeners keep the approved version. One submission waits per
-- release; a newer change joins it and raises its revision, so a decision on what was seen earlier is refused.
CREATE TABLE release_reviews (
    id           text PRIMARY KEY DEFAULT 'rev_' || replace(gen_random_uuid()::text, '-', ''),
    seq          bigserial,
    release_id   text NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('new', 'update')),
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'withdrawn')),
    revision     integer NOT NULL DEFAULT 1,
    changes      jsonb NOT NULL DEFAULT '{}',
    publish_at   timestamptz, -- when the artist asked for a new release to go live
    submitted_at timestamptz NOT NULL DEFAULT now(),
    decided_at   timestamptz,
    decided_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    reason       text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX release_reviews_one_pending ON release_reviews (release_id) WHERE status = 'pending';
CREATE INDEX release_reviews_release_idx ON release_reviews (release_id, seq DESC);
CREATE INDEX release_reviews_queue_idx ON release_reviews (submitted_at) WHERE status = 'pending';

-- A song added to a release that is already online waits, hidden, for approval: making a release public skips it.
ALTER TABLE tracks ADD COLUMN review_hold boolean NOT NULL DEFAULT false;
