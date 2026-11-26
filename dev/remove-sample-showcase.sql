-- DEV ONLY: removes what dev/sample-showcase.sql added (rows whose consent note starts with "DEV SAMPLE").
--
--   docker compose exec -T postgres psql -U blac -d blac -f - < dev/remove-sample-showcase.sql
DELETE FROM showcase_items WHERE consent_note LIKE 'DEV SAMPLE%';
