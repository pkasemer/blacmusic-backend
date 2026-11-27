-- Catalog artists can be handed to a user and taken back.

-- Uploads and pictures can outlive the account that made them: songs and covers of an artist stay when the person
-- (a former owner, or an administrator who added them) deletes their account. Before this, deleting an account
-- deleted every upload and picture it had ever made. Account deletion now chooses what to remove.
ALTER TABLE uploads ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE uploads DROP CONSTRAINT uploads_user_id_fkey,
    ADD CONSTRAINT uploads_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE images ALTER COLUMN owner_id DROP NOT NULL;
ALTER TABLE images DROP CONSTRAINT images_owner_id_fkey,
    ADD CONSTRAINT images_owner_id_fkey FOREIGN KEY (owner_id) REFERENCES users(id) ON DELETE SET NULL;

-- by_team: an administrator uploaded it for the artist. Set by the server only (the terms version is sent by apps,
-- so it cannot be trusted for this). quota_exempt: it does not count against the artist's storage allowance.
ALTER TABLE uploads
    ADD COLUMN by_team      boolean NOT NULL DEFAULT false,
    ADD COLUMN quota_exempt boolean NOT NULL DEFAULT false;
UPDATE uploads u SET by_team = true
    WHERE u.rights_terms_version = 'admin' AND EXISTS (SELECT 1 FROM users a WHERE a.id = u.user_id AND a.role = 'admin');
UPDATE uploads u SET quota_exempt = true
    WHERE u.by_team AND EXISTS (SELECT 1 FROM artists a WHERE a.id = u.artist_id AND a.owner_user_id IS NULL);

-- assigned_at: the owner received this artist from the catalog (NULL: they created it, or it has no owner). A
-- received artist goes back to the catalog when its owner's account is deleted.
ALTER TABLE artists
    ADD COLUMN assigned_at timestamptz,
    ADD COLUMN assigned_by uuid REFERENCES users(id) ON DELETE SET NULL;
