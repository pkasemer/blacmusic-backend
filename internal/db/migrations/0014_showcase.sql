-- The Blac team's hand-picked artists and albums shown to signed-out visitors (the app's Get started
-- screens). Every entry records how the owner agreed to appear, and who added it.

CREATE TABLE showcase_items (
    position     integer NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('artist', 'album')),
    ref_id       text NOT NULL,
    consent_note text NOT NULL,
    added_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    added_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (position),
    UNIQUE (kind, ref_id)
);
