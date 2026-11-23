-- Artist mode: verified email, artist profiles owned by users, releases with
-- draft/scheduled/public visibility, images (avatars and covers), a storage
-- quota ledger, and rights acceptance on uploads.

-- ---------------------------------------------------------------- email codes
ALTER TABLE users ADD COLUMN email_verified_at timestamptz;

-- One live code per user; sending a new one replaces it.
CREATE TABLE email_codes (
    user_id    uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    code_hash  text        NOT NULL,
    expires_at timestamptz NOT NULL,
    attempts   integer     NOT NULL DEFAULT 0,
    sent_at    timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------- artist profiles
-- Existing rows (catalog artists with no account) keep owner_user_id NULL.
ALTER TABLE artists
    ADD COLUMN owner_user_id             uuid UNIQUE REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN bio                       text    NOT NULL DEFAULT '',
    ADD COLUMN avatar_image_id           text,
    ADD COLUMN verification              text    NOT NULL DEFAULT 'unverified'
        CHECK (verification IN ('unverified', 'pending', 'verified', 'rejected')),
    ADD COLUMN verification_message      text    NOT NULL DEFAULT '', -- what the artist told us when asking
    ADD COLUMN verification_note         text    NOT NULL DEFAULT '', -- the admin's reason when rejecting
    ADD COLUMN verification_requested_at timestamptz,
    ADD COLUMN flagged                   boolean NOT NULL DEFAULT false, -- name matches a verified artist
    ADD COLUMN updated_at                timestamptz NOT NULL DEFAULT now();
CREATE INDEX artists_verification_idx ON artists (verification) WHERE verification = 'pending';
CREATE INDEX artists_flagged_idx ON artists (created_at) WHERE flagged;

-- Storage quota as a ledger, so payments, refunds and promotions are rows
-- rather than edits to a counter. A positive grant adds space; a negative
-- 'adjustment' removes it. `reference` (e.g. a payment id) makes grants idempotent.
CREATE TABLE storage_grants (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    artist_id  text    NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    kind       text    NOT NULL CHECK (kind IN ('free', 'purchase', 'promo', 'adjustment')),
    bytes      bigint  NOT NULL CHECK (bytes <> 0),
    note       text    NOT NULL DEFAULT '',
    reference  text,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX storage_grants_artist_idx ON storage_grants (artist_id);
CREATE UNIQUE INDEX storage_grants_reference_idx ON storage_grants (reference) WHERE reference IS NOT NULL;

-- ---------------------------------------------------------------------- images
-- Avatars and cover art. The raw upload is deleted once it has been
-- validated and re-encoded into the stored sizes (EXIF and other metadata are dropped).
CREATE TABLE images (
    id           text PRIMARY KEY DEFAULT 'img_' || replace(gen_random_uuid()::text, '-', ''),
    owner_id     uuid   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose      text   NOT NULL CHECK (purpose IN ('avatar', 'cover')),
    status       text   NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'failed')),
    original_key text   NOT NULL,
    size_bytes   bigint NOT NULL,          -- declared size of the raw upload
    width        integer,
    height       integer,
    error        text   NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX images_owner_idx ON images (owner_id, created_at DESC);

-- -------------------------------------------------------------------- releases
-- A release is an album row. Existing albums stay public.
ALTER TABLE albums
    ADD COLUMN release_type  text NOT NULL DEFAULT 'album' CHECK (release_type IN ('single', 'album')),
    ADD COLUMN description   text NOT NULL DEFAULT '',
    ADD COLUMN genre         text NOT NULL DEFAULT '',
    ADD COLUMN cover_image_id text,
    ADD COLUMN visibility    text NOT NULL DEFAULT 'public' CHECK (visibility IN ('draft', 'scheduled', 'public')),
    ADD COLUMN publish_at    timestamptz,   -- when a scheduled release goes live
    ADD COLUMN published_at  timestamptz,
    ADD COLUMN updated_at    timestamptz NOT NULL DEFAULT now();
CREATE INDEX albums_artist_idx ON albums (artist_id);
CREATE INDEX albums_scheduled_idx ON albums (publish_at) WHERE visibility = 'scheduled';

-- A track is visible to everyone only when it is 'public'; the release's
-- publish step flips its tracks together. Existing tracks stay public.
ALTER TABLE tracks
    ADD COLUMN visibility   text NOT NULL DEFAULT 'public' CHECK (visibility IN ('draft', 'public')),
    ADD COLUMN genre        text NOT NULL DEFAULT '',
    ADD COLUMN track_number integer;
CREATE INDEX tracks_album_idx ON tracks (album_id, track_number);
CREATE INDEX tracks_artist_idx ON tracks (artist_id);

-- --------------------------------------------------------------------- uploads
-- artist_id/release_id are NULL only on uploads from before artist mode.
ALTER TABLE uploads
    ADD COLUMN artist_id            text REFERENCES artists(id) ON DELETE SET NULL,
    ADD COLUMN release_id           text REFERENCES albums(id)  ON DELETE SET NULL,
    ADD COLUMN genre                text NOT NULL DEFAULT '',
    ADD COLUMN track_number         integer,
    ADD COLUMN rights_terms_version text NOT NULL DEFAULT '',
    ADD COLUMN rights_accepted_at   timestamptz;
CREATE INDEX uploads_artist_idx ON uploads (artist_id, status);
CREATE INDEX uploads_release_idx ON uploads (release_id);
