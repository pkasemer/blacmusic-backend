-- DEV ONLY: removes everything dev/sample-playlists.sql created. Deleting the
-- seeded account removes its playlists, their items and anyone's saves of them.
--
--   docker compose exec -T postgres psql -U blac -d blac -f - < dev/remove-sample-playlists.sql
DELETE FROM users WHERE email = 'sample-playlists@blac.test';
