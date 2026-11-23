-- Dev seed: one artist, album and track pointing at media_key 'demo/sample.mp3'.
-- Upload any mp3 to the blac-media bucket at that key (storage console: http://localhost:9001,
-- login blacminio / blacminio-secret) to test playback.
--
--   docker compose exec -T postgres psql -U blac -d blac < scripts/seed.sql

INSERT INTO artists (id, name) VALUES ('art_demo', 'Demo Artist') ON CONFLICT DO NOTHING;
INSERT INTO albums  (id, title, artist_id) VALUES ('alb_demo', 'Demo Album', 'art_demo') ON CONFLICT DO NOTHING;
INSERT INTO tracks  (id, title, artist_id, album_id, duration_ms)
    VALUES ('trk_demo', 'Demo Track', 'art_demo', 'alb_demo', 30000) ON CONFLICT DO NOTHING;
INSERT INTO track_sources (track_id, source, source_id, media_key, codec, bitrate_kbps)
    VALUES ('trk_demo', 'native', 'trk_demo', 'demo/sample.mp3', 'mp3', 128) ON CONFLICT DO NOTHING;
