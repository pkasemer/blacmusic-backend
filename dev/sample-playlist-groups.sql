-- DEV ONLY sample data: the Home shelves ("Today's biggest hits", "Workout", ...)
-- filled with the four sample playlists from dev/sample-playlists.sql (run that
-- first). Never run this against production.
--
--   docker compose exec -T postgres psql -U blac -d blac -f - < dev/sample-playlist-groups.sql
--
-- Romance, Focus and Chill are left without playlists on purpose: empty groups
-- must not show up in GET /v1/browse/playlist-groups.
-- Safe to run again (it resets these groups' members). Remove with
-- dev/remove-sample-playlist-groups.sql.
\set ON_ERROR_STOP on

BEGIN;

INSERT INTO playlist_groups (id, title, description, position, is_active) VALUES
  ('todays-biggest-hits', 'Today''s biggest hits', 'The songs everyone is playing right now.', 10, true),
  ('throwback',           'Throwback',             'Classics that never get old.',            20, true),
  ('workout',             'Workout',               'Tracks to keep the energy up.',           30, true),
  ('party',               'Party',                 'Turn it up.',                             40, true),
  ('romance',             'Romance',               'For the two of you.',                     50, true),
  ('focus',               'Focus',                 'Calm background for deep work.',          60, true),
  ('chill',               'Chill',                 'Easy listening.',                         70, true)
ON CONFLICT (id) DO UPDATE
  SET title = EXCLUDED.title, description = EXCLUDED.description, position = EXCLUDED.position, is_active = true;

DELETE FROM playlist_group_items
WHERE group_id IN ('todays-biggest-hits', 'throwback', 'workout', 'party', 'romance', 'focus', 'chill');

WITH members(group_id, playlist_name, position) AS (VALUES
  ('todays-biggest-hits', 'Afrobeats Heat',   0),
  ('todays-biggest-hits', 'Late Night Drive', 1),
  ('todays-biggest-hits', 'Gym Energy',       2),
  ('throwback',           'Sunday Soul',      0),
  ('throwback',           'Late Night Drive', 1),
  ('workout',             'Gym Energy',       0),
  ('workout',             'Afrobeats Heat',   1),
  ('party',               'Afrobeats Heat',   0),
  ('party',               'Gym Energy',       1)
)
INSERT INTO playlist_group_items (group_id, playlist_id, position)
SELECT m.group_id, p.id, m.position
FROM members m
JOIN users u ON u.email = 'sample-playlists@blac.test'
JOIN playlists p ON p.owner_id = u.id AND p.name = m.playlist_name;

COMMIT;

SELECT g.position, g.id, g.title, count(i.playlist_id) AS playlists
FROM playlist_groups g LEFT JOIN playlist_group_items i ON i.group_id = g.id
WHERE g.id IN ('todays-biggest-hits', 'throwback', 'workout', 'party', 'romance', 'focus', 'chill')
GROUP BY g.id ORDER BY g.position;
