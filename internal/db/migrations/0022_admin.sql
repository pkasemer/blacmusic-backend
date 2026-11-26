-- Administration: banning users and artists, taking releases down, and an append-only log of what
-- administrators did.

ALTER TABLE users
    ADD COLUMN banned_at  timestamptz,
    ADD COLUMN ban_reason text NOT NULL DEFAULT '',
    ADD COLUMN banned_by  uuid;
CREATE INDEX users_banned_idx ON users (banned_at) WHERE banned_at IS NOT NULL;

ALTER TABLE artists
    ADD COLUMN banned_at  timestamptz,
    ADD COLUMN ban_reason text NOT NULL DEFAULT '',
    ADD COLUMN banned_by  uuid;
CREATE INDEX artists_banned_idx ON artists (banned_at) WHERE banned_at IS NOT NULL;

-- A taken-down release is hidden like a draft (visibility = 'draft'); these columns remember what it was so
-- that restoring puts it back, and stop the owner from publishing it again.
ALTER TABLE albums
    ADD COLUMN taken_down_at              timestamptz,
    ADD COLUMN takedown_reason            text NOT NULL DEFAULT '',
    ADD COLUMN taken_down_by              uuid,
    ADD COLUMN taken_down_prev_visibility text,
    ADD COLUMN taken_down_prev_publish_at timestamptz,
    ADD COLUMN taken_down_by_ban          boolean NOT NULL DEFAULT false;
CREATE INDEX albums_taken_down_idx ON albums (taken_down_at) WHERE taken_down_at IS NOT NULL;

-- Playlists made private because their owner was banned (unbanning makes them public again).
ALTER TABLE playlists ADD COLUMN hidden_by_ban boolean NOT NULL DEFAULT false;

-- Append-only: rows are written in the same transaction as the change they record; actor_email and
-- target_label are copied so the log still reads after an account is deleted.
CREATE TABLE admin_audit_log (
    id           bigserial PRIMARY KEY,
    at           timestamptz NOT NULL DEFAULT now(),
    actor_user_id uuid,
    actor_email  text NOT NULL DEFAULT '',
    action       text NOT NULL,
    target_type  text NOT NULL DEFAULT '',
    target_id    text NOT NULL DEFAULT '',
    target_label text NOT NULL DEFAULT '',
    reason       text NOT NULL DEFAULT '',
    details      jsonb NOT NULL DEFAULT '{}',
    ip           text NOT NULL DEFAULT ''
);
CREATE INDEX admin_audit_log_at_idx ON admin_audit_log (at DESC, id DESC);
CREATE INDEX admin_audit_log_target_idx ON admin_audit_log (target_type, target_id);
CREATE INDEX admin_audit_log_actor_idx ON admin_audit_log (actor_user_id);
CREATE INDEX admin_audit_log_action_idx ON admin_audit_log (action);

CREATE FUNCTION admin_audit_log_readonly() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'admin_audit_log is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER admin_audit_log_no_change BEFORE UPDATE OR DELETE ON admin_audit_log
    FOR EACH ROW EXECUTE FUNCTION admin_audit_log_readonly();
CREATE TRIGGER admin_audit_log_no_truncate BEFORE TRUNCATE ON admin_audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION admin_audit_log_readonly();
