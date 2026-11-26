-- Named, ordered groups of editorial playlists ("Workout", "Chill", ...): one
-- shelf each on the Home screen. A playlist can be in several groups. The team
-- manages them through /v1/admin/playlist-groups.

CREATE TABLE playlist_groups (
    id          text PRIMARY KEY,
    title       text NOT NULL,
    description text NOT NULL DEFAULT '',
    position    integer NOT NULL DEFAULT 0,
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX playlist_groups_order_idx ON playlist_groups (position, id) WHERE is_active;

CREATE TABLE playlist_group_items (
    group_id    text NOT NULL REFERENCES playlist_groups(id) ON DELETE CASCADE,
    playlist_id text NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    position    integer NOT NULL,
    PRIMARY KEY (group_id, playlist_id)
);
CREATE INDEX playlist_group_items_order_idx ON playlist_group_items (group_id, position);
CREATE INDEX playlist_group_items_playlist_idx ON playlist_group_items (playlist_id);
