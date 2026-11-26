-- Creates the Blac Music editorial playlists, empty and featured, owned by one
-- dedicated account. The team then adds tracks by signing in to that account
-- in the app (or with the playlist API). A playlist appears under Browse once
-- it holds at least one public track.
--
-- Safe to run again: playlists that already exist (same owner and name) are
-- left alone, so edits made since are kept.
--
--   psql -v owner_email=editorial@yourdomain.com [-v display_name="Blac Music"] -f editorial.sql
--
-- The owner must be a registered account used only for editorial work: its
-- display name becomes the name shown on every playlist.
\set ON_ERROR_STOP on
\if :{?display_name}
\else
\set display_name 'Blac Music'
\endif

BEGIN;

UPDATE users SET display_name = :'display_name' WHERE email = :'owner_email';

INSERT INTO playlists (owner_id, name, description, is_public, is_featured, featured_at)
SELECT u.id, v.name, v.description, true, true, now() - (v.rank * interval '1 second')
FROM users u
CROSS JOIN (VALUES
  (1,  'New Music Uganda',   'Fresh releases from Ugandan artists, updated every week.'),
  (2,  'Kampala Nights',     'The sound of the city after dark: afrobeat, dancehall and amapiano.'),
  (3,  'Luganda Hits',       'The biggest Luganda songs right now.'),
  (4,  'Gospel Praise',      'Praise and worship from across Uganda.'),
  (5,  'Afrobeat Fire',      'Afrobeats heat from Kampala to Lagos.'),
  (6,  'Dancehall Ug',       'Ugandan dancehall: riddims, anthems and crowd favourites.'),
  (7,  'Hip Hop Kampala',    'Bars and beats from the Kampala scene.'),
  (8,  'Chill Evenings',     'Slow and easy: R&B, soul and acoustic for winding down.'),
  (9,  'Throwback Uganda',   'The classics that raised a generation.'),
  (10, 'Boda Boda Rides',    'Upbeat tracks for the ride across town.'),
  (11, 'Love Songs',         'Romantic Ugandan songs for every mood.'),
  (12, 'Rising Artists',     'New voices to watch, picked by the Blac Music team.')
) AS v(rank, name, description)
WHERE u.email = :'owner_email'
  AND NOT EXISTS (SELECT 1 FROM playlists p WHERE p.owner_id = u.id AND p.name = v.name);

COMMIT;

SELECT u.display_name AS owner, count(p.id) AS playlists, count(*) FILTER (WHERE p.is_featured) AS featured
FROM users u LEFT JOIN playlists p ON p.owner_id = u.id
WHERE u.email = :'owner_email'
GROUP BY u.id;
