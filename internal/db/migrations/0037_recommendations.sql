-- The data the recommendation engine (internal/recommend) reads, and the record of what it suggested.

-- What it needs to know about each song people can play. Language, region, culture and the sound attributes count
-- only once verified: an unchecked or rejected value says nothing, and nothing is worked out from anything else.
-- The genre is what the uploader chose (the one every song has), plus the genres on the song that were not rejected.
CREATE VIEW rec_track_features AS
       -- a song without an artist counts as its own, so such songs are not held to one artist's share between them
SELECT t.id AS track_id, COALESCE(t.artist_id, 'none:' || t.id) AS artist_id, a.owner_user_id AS artist_owner,
       COALESCE(al.release_date::timestamptz, al.published_at, t.created_at) AS released_at,
       -- the list's id when the genre is one of its names, else the name itself; none if the team rejected it
       CASE WHEN EXISTS (SELECT 1 FROM track_genres r WHERE r.track_id = t.id AND r.genre_id = g.id AND r.verification = 'rejected') THEN ''
            ELSE COALESCE(g.id, lower(COALESCE(NULLIF(t.genre, ''), al.genre, ''))) END AS genre,
       ARRAY(SELECT x.genre_id FROM track_genres x WHERE x.track_id = t.id AND x.verification <> 'rejected') AS genres,
       ARRAY(SELECT x.language_id FROM track_languages x WHERE x.track_id = t.id AND x.verification = 'verified' AND x.role = 'primary') AS primary_languages,
       ARRAY(SELECT x.language_id FROM track_languages x WHERE x.track_id = t.id AND x.verification = 'verified' AND x.role = 'secondary') AS other_languages,
       ARRAY(SELECT x.region_id FROM track_regions x WHERE x.track_id = t.id AND x.verification = 'verified') AS regions,
       ARRAY(SELECT x.culture_id FROM track_cultures x WHERE x.track_id = t.id AND x.verification = 'verified') AS cultures,
       ARRAY(SELECT x.kind || ':' || x.attribute_id FROM track_attributes x WHERE x.track_id = t.id AND x.verification = 'verified') AS attributes
FROM tracks t
LEFT JOIN artists a ON a.id = t.artist_id
LEFT JOIN albums al ON al.id = t.album_id
LEFT JOIN tax_genres g ON lower(g.label) = lower(COALESCE(NULLIF(t.genre, ''), al.genre))
WHERE t.visibility = 'public' AND a.banned_at IS NULL AND EXISTS (SELECT 1 FROM track_sources s WHERE s.track_id = t.id AND s.source = 'native');

-- Each listener's history with each song: the one definition of what they did. Read for one listener at a time, so
-- a like or a skip counts at once. Weights (the _w columns) halve every 45 days; only the last 180 days of listening
-- count. Skips measured the app's first way (capture under 2) include false ones, so they are kept apart.
CREATE VIEW rec_user_tracks AS
SELECT user_id, track_id,
       sum(listens)::int AS listens,
       sum(completes)::int AS completes, sum(completes_w)::float8 AS completes_w,
       sum(replays_w)::float8 AS replays_w,
       sum(skips)::int AS skips, sum(skips_w)::float8 AS skips_w,
       sum(weak_skips)::int AS weak_skips, sum(weak_skips_w)::float8 AS weak_skips_w,
       sum(plays_w)::float8 AS plays_w,
       sum(playlist_adds)::int AS playlist_adds,
       bool_or(liked) AS liked, bool_or(unliked) AS unliked, bool_or(saved) AS saved,
       max(heard_at) AS last_heard_at, max(skipped_at) AS last_skipped_at, max(weak_skipped_at) AS last_weak_skipped_at
FROM (
    SELECT user_id, track_id, count(*) AS listens,
           count(*) FILTER (WHERE outcome = 'completed') AS completes,
           COALESCE(sum(w) FILTER (WHERE outcome = 'completed'), 0) AS completes_w,
           COALESCE(sum(w) FILTER (WHERE outcome = 'completed' AND replay), 0) AS replays_w,
           count(*) FILTER (WHERE outcome = 'skipped' AND capture >= 2) AS skips,
           -- leaving a song almost at once says more than leaving it late
           COALESCE(sum(w * CASE WHEN completion < 30 THEN 1.0 ELSE 0.5 END) FILTER (WHERE outcome = 'skipped' AND capture >= 2), 0) AS skips_w,
           count(*) FILTER (WHERE outcome = 'skipped' AND capture < 2) AS weak_skips,
           COALESCE(sum(w) FILTER (WHERE outcome = 'skipped' AND capture < 2), 0) AS weak_skips_w,
           0::float8 AS plays_w, 0 AS playlist_adds, false AS liked, false AS unliked, false AS saved,
           max(at) AS heard_at,
           max(at) FILTER (WHERE outcome = 'skipped' AND capture >= 2) AS skipped_at,
           max(at) FILTER (WHERE outcome = 'skipped' AND capture < 2) AS weak_skipped_at
    FROM (SELECT e.*, exp(-0.6931 * GREATEST(extract(epoch FROM now() - e.at), 0) / 86400 / 45) AS w
          FROM listening_events e WHERE e.type = 'listen' AND e.at > now() - interval '180 days') l
    GROUP BY user_id, track_id
    UNION ALL -- the latest tap on the heart
    SELECT user_id, track_id, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, type = 'like', type = 'unlike', false, NULL, NULL, NULL
    FROM (SELECT DISTINCT ON (user_id, track_id) user_id, track_id, type FROM listening_events
          WHERE type IN ('like', 'unlike') ORDER BY user_id, track_id, at DESC) k
    UNION ALL
    SELECT user_id, track_id, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, false, false, true, NULL, NULL, NULL FROM library_tracks
    UNION ALL -- songs they put in their own playlists and left there; not what the app or the server put in
    SELECT p.owner_id, i.track_id, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, false, false, false, NULL, NULL, NULL
    FROM playlists p JOIN playlist_items i ON i.playlist_id = p.id AND i.added_by = p.owner_id AND NOT i.by_server
    WHERE p.name <> 'Your sound'
    UNION ALL -- counted plays: older than the listening events, and from apps that send none
    SELECT user_id, track_id, 0, 0, 0, 0, 0, 0, 0, 0,
           exp(-0.6931 * GREATEST(extract(epoch FROM now() - at), 0) / 86400 / 45), 0, false, false, false, at, NULL, NULL
    FROM plays WHERE user_id IS NOT NULL AND at > now() - interval '180 days'
) x
WHERE track_id <> ''
GROUP BY user_id, track_id;

-- How each song is doing with listeners as a whole. Rebuilt by the worker from the signal_* views, so staff, test
-- accounts and artists' own listening do not count. A listener counts once a day however often they repeat a song.
CREATE TABLE rec_track_stats (
    track_id     text PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    listeners_7d integer NOT NULL DEFAULT 0,
    listeners_30d integer NOT NULL DEFAULT 0,
    completes_90d integer NOT NULL DEFAULT 0, -- listener-days
    skips_90d     integer NOT NULL DEFAULT 0, -- listener-days, trusted skips only
    savers        integer NOT NULL DEFAULT 0
);
CREATE INDEX rec_track_stats_popular_idx ON rec_track_stats (listeners_30d DESC);
CREATE INDEX listening_events_track_idx ON listening_events (track_id, at) WHERE type = 'listen';
CREATE INDEX track_regions_term_idx ON track_regions (region_id);
CREATE INDEX track_cultures_term_idx ON track_cultures (culture_id);

-- What was suggested, where, why and by which version, so the engine can be judged by what people then did. Goes
-- with the account. The worker deletes rows after 90 days.
CREATE TABLE rec_serves (
    id            bigserial PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    surface       text NOT NULL CHECK (surface IN ('mix', 'autoplay', 'your_sound')),
    algorithm     text NOT NULL,
    discovery_mode text NOT NULL DEFAULT '',
    seed_track_id text NOT NULL DEFAULT '',
    at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rec_serves_user_idx ON rec_serves (user_id, at DESC);
CREATE INDEX rec_serves_at_idx ON rec_serves (at);
CREATE TABLE rec_serve_items (
    serve_id bigint NOT NULL REFERENCES rec_serves(id) ON DELETE CASCADE,
    position integer NOT NULL,
    track_id text NOT NULL,
    slot     text NOT NULL CHECK (slot IN ('familiar', 'discovery')),
    score    real NOT NULL,
    reasons  text[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (serve_id, position)
);

-- What happened to each suggestion: the first listen of that song soon after, from where the suggestion would be
-- played (apps measuring the first way cannot say where, so any listen counts for them), and whether it was then
-- liked, saved or added to a playlist. Staff and test accounts are left out.
CREATE VIEW rec_outcomes AS
SELECT s.id AS serve_id, s.user_id, s.surface, s.algorithm, s.discovery_mode, s.at, i.position, i.track_id, i.slot, i.reasons,
       l.outcome, l.completion,
       EXISTS (SELECT 1 FROM listening_events k WHERE k.user_id = s.user_id AND k.track_id = i.track_id
               AND k.type IN ('like', 'playlist_add') AND k.at >= s.at AND k.at < s.at + w.span) AS kept
FROM rec_serves s
JOIN rec_serve_items i ON i.serve_id = s.id
CROSS JOIN LATERAL (SELECT CASE WHEN s.surface = 'your_sound' THEN interval '7 days' ELSE interval '48 hours' END AS span) w
LEFT JOIN LATERAL (
    SELECT e.outcome, e.completion FROM listening_events e
    WHERE e.user_id = s.user_id AND e.track_id = i.track_id AND e.type = 'listen' AND e.at >= s.at AND e.at < s.at + w.span
      AND (e.capture < 2 OR e.source = CASE s.surface WHEN 'your_sound' THEN 'playlist' ELSE s.surface END)
    ORDER BY e.at LIMIT 1
) l ON true
WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = s.user_id);
