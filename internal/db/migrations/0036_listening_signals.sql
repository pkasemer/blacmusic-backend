-- Better listening signals.
--
-- How a listen ended, so "left it for another song" can be told apart from a song that finished, an app that closed
-- or a change nobody saw. capture is the app's way of measuring: 0 is the first one, whose skips include false ones.
ALTER TABLE listening_events
    ADD COLUMN end_reason       text NOT NULL DEFAULT '',
    ADD COLUMN end_position_sec integer NOT NULL DEFAULT 0,
    ADD COLUMN seed_track_id    text NOT NULL DEFAULT '', -- for source 'autoplay': the song it was chosen from
    ADD COLUMN capture          smallint NOT NULL DEFAULT 0;

-- Staff and test accounts: what they do must not shape what other people are offered. Administrators are always left
-- out; the team marks the rest.
ALTER TABLE users
    ADD COLUMN signals_excluded_at timestamptz,
    ADD COLUMN signals_excluded_by uuid;

CREATE VIEW signal_excluded_users AS
    SELECT id FROM users WHERE role = 'admin' OR signals_excluded_at IS NOT NULL;

-- Everything that ranks or recommends from what many people did reads these, never the tables themselves. A listener's
-- own recommendations still use their own activity. Numbers shown to artists and admins are not filtered.
CREATE VIEW signal_plays AS
    SELECT p.* FROM plays p WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = p.user_id);
CREATE VIEW signal_saves AS
    SELECT l.* FROM library_tracks l WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = l.user_id);
CREATE VIEW signal_album_saves AS
    SELECT l.* FROM library_albums l WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = l.user_id);
CREATE VIEW signal_follows AS
    SELECT l.* FROM library_artists l WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = l.user_id);
CREATE VIEW signal_playlist_saves AS
    SELECT l.* FROM library_playlists l WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = l.user_id);
-- For models and reports across listeners: no excluded accounts, and no artist listening to their own music.
CREATE VIEW signal_listens AS
    SELECT e.* FROM listening_events e
    WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = e.user_id)
      AND NOT EXISTS (SELECT 1 FROM tracks t JOIN artists a ON a.id = t.artist_id
                      WHERE t.id = e.track_id AND a.owner_user_id = e.user_id);
