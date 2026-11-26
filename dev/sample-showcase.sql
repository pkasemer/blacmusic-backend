-- DEV ONLY sample data: fills the signed-out showcase (GET /v1/showcase) from the local sample
-- catalogue, so the app's Get started screens have pictures to show. Never run this against
-- production: the consent notes here are placeholders, not real consent records.
--
--   docker compose exec -T postgres psql -U blac -d blac -f - < dev/sample-showcase.sql
--
-- It picks up to 3 artists that have a profile picture and a public release, and up to 3 public
-- albums that have a cover. Safe to run again. Remove with dev/remove-sample-showcase.sql.
\set ON_ERROR_STOP on

BEGIN;

DELETE FROM showcase_items WHERE consent_note LIKE 'DEV SAMPLE%';

WITH picks AS (
  (SELECT 'artist' AS kind, a.id AS ref_id, 1 AS grp
     FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
    WHERE (u.avatar_image_id IS NOT NULL OR COALESCE(a.image_url, '') <> '')
      AND EXISTS (SELECT 1 FROM albums al WHERE al.artist_id = a.id AND al.visibility = 'public'
                  AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public'))
    ORDER BY a.name LIMIT 3)
  UNION ALL
  (SELECT 'album', al.id, 2
     FROM albums al
    WHERE al.visibility = 'public' AND COALESCE(al.artwork_url, '') <> ''
      AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
    ORDER BY al.title LIMIT 3)
), numbered AS (
  SELECT kind, ref_id, row_number() OVER (ORDER BY grp, ref_id) AS n FROM picks
)
INSERT INTO showcase_items (position, kind, ref_id, consent_note)
SELECT n + COALESCE((SELECT max(position) FROM showcase_items), 0), kind, ref_id, 'DEV SAMPLE: placeholder, not a real consent record'
FROM numbered
ON CONFLICT (kind, ref_id) DO NOTHING;

COMMIT;

SELECT position, kind, ref_id FROM showcase_items ORDER BY position;
