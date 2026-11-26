-- DEV ONLY sample data: a few public playlists so the app's Home "Playlists"
-- row has something to show. Never run this against production.
--
--   docker compose exec -T postgres psql -U blac -d blac -f - < dev/sample-playlists.sql
--
-- It uses the public tracks already in the local database (the same tracks are
-- reused across playlists) and a seeded account "Blac" that cannot sign in.
-- Safe to run again. Remove everything with dev/remove-sample-playlists.sql.
\set ON_ERROR_STOP on

BEGIN;

INSERT INTO users (email, password_hash, display_name)
SELECT 'sample-playlists@blac.test', '!', 'Blac'
WHERE NOT EXISTS (SELECT 1 FROM users WHERE email = 'sample-playlists@blac.test');

INSERT INTO playlists (owner_id, name, description, is_public, is_featured, featured_at)
SELECT u.id, v.name, v.description, true, v.featured, CASE WHEN v.featured THEN now() - v.rank * interval '1 minute' END
FROM users u
CROSS JOIN (VALUES
  (1, 'Afrobeats Heat',  'Hot afrobeats to get the day moving.',        true),
  (2, 'Late Night Drive','Smooth tracks for the road after dark.',      true),
  (3, 'Sunday Soul',     'Slow, warm and easy for a lazy Sunday.',      false),
  (4, 'Gym Energy',      'High tempo tracks to push through the set.',  false)
) AS v(rank, name, description, featured)
WHERE u.email = 'sample-playlists@blac.test'
  AND NOT EXISTS (SELECT 1 FROM playlists p WHERE p.owner_id = u.id AND p.name = v.name);

-- Fill each empty sample playlist from the public tracks (by a stable order).
WITH ranked AS (
  SELECT id, row_number() OVER (ORDER BY id) AS rn FROM tracks WHERE visibility = 'public'
), picks(name, rns) AS (VALUES
  ('Afrobeats Heat',   ARRAY[1,2,3,4]),
  ('Late Night Drive', ARRAY[5,6,7,8]),
  ('Sunday Soul',      ARRAY[3,7,9]),
  ('Gym Energy',       ARRAY[2,4,6])
)
INSERT INTO playlist_items (playlist_id, track_id, position)
SELECT p.id, r.id, x.ord - 1
FROM picks
JOIN users u ON u.email = 'sample-playlists@blac.test'
JOIN playlists p ON p.owner_id = u.id AND p.name = picks.name
CROSS JOIN LATERAL unnest(picks.rns) WITH ORDINALITY AS x(rn, ord)
JOIN ranked r ON r.rn = x.rn
WHERE NOT EXISTS (SELECT 1 FROM playlist_items i WHERE i.playlist_id = p.id);

COMMIT;

SELECT p.name, p.is_featured, count(i.id) AS tracks
FROM users u JOIN playlists p ON p.owner_id = u.id LEFT JOIN playlist_items i ON i.playlist_id = p.id
WHERE u.email = 'sample-playlists@blac.test' GROUP BY p.id ORDER BY p.name;
