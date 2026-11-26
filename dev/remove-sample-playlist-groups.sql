-- DEV ONLY: removes the groups created by dev/sample-playlist-groups.sql (and
-- their memberships). The playlists themselves stay; remove those with
-- dev/remove-sample-playlists.sql.
--
--   docker compose exec -T postgres psql -U blac -d blac -f - < dev/remove-sample-playlist-groups.sql
DELETE FROM playlist_groups
WHERE id IN ('todays-biggest-hits', 'throwback', 'workout', 'party', 'romance', 'focus', 'chill');
