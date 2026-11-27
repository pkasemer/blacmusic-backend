-- A song waits for review as soon as what is required is filled in and not yet verified, however it got there: a save,
-- a bulk change or a submission. Before, only a song submitted one at a time counted, so the queue stayed empty.
-- Songs nobody has touched (an imported genre only) are still not started.
CREATE OR REPLACE VIEW track_metadata_state AS
SELECT f.track_id, f.has_language, f.language_unknown, f.unknown_verification, f.language_pending, f.has_genre,
       f.culture_pending, f.region_pending, x.missing,
       CASE
           WHEN NOT f.touched THEN 'not_started'
           WHEN cardinality(x.missing) = 0 AND NOT (
                    ('language' = ANY(f.required) AND (f.language_pending OR (f.language_unknown AND f.unknown_verification = 'pending')))
                 OR ('genre' = ANY(f.required) AND f.genre_pending)
                 OR ('culture' = ANY(f.required) AND f.culture_pending)
                 OR ('region' = ANY(f.required) AND f.region_pending)) THEN 'verified'
           WHEN cardinality(x.missing) = 0 THEN 'pending_review'
           ELSE 'in_progress'
       END AS status
FROM (
    SELECT t.id AS track_id, s.required_fields AS required,
           COALESCE(m.language_unknown, false) AS language_unknown,
           COALESCE(m.unknown_verification, 'pending') AS unknown_verification,
           m.submitted_at IS NOT NULL AS submitted,
           EXISTS (SELECT 1 FROM track_languages l WHERE l.track_id = t.id AND l.verification <> 'rejected') AS has_language,
           EXISTS (SELECT 1 FROM track_languages l WHERE l.track_id = t.id AND l.verification = 'pending') AS language_pending,
           EXISTS (SELECT 1 FROM track_genres g WHERE g.track_id = t.id AND g.verification <> 'rejected') AS has_genre,
           EXISTS (SELECT 1 FROM track_genres g WHERE g.track_id = t.id AND g.verification = 'pending') AS genre_pending,
           EXISTS (SELECT 1 FROM track_cultures c WHERE c.track_id = t.id AND c.verification <> 'rejected') AS has_culture,
           EXISTS (SELECT 1 FROM track_cultures c WHERE c.track_id = t.id AND c.verification = 'pending') AS culture_pending,
           EXISTS (SELECT 1 FROM track_regions r WHERE r.track_id = t.id AND r.verification <> 'rejected') AS has_region,
           EXISTS (SELECT 1 FROM track_regions r WHERE r.track_id = t.id AND r.verification = 'pending') AS region_pending,
           -- started by the team: something of its own, or something it has reviewed or sent for review
           COALESCE(m.language_unknown, false) OR m.submitted_at IS NOT NULL OR EXISTS (
               SELECT 1 FROM (
                   SELECT track_id, source, verification FROM track_languages UNION ALL
                   SELECT track_id, source, verification FROM track_cultures UNION ALL
                   SELECT track_id, source, verification FROM track_regions UNION ALL
                   SELECT track_id, source, verification FROM track_genres UNION ALL
                   SELECT track_id, source, verification FROM track_attributes) v
               WHERE v.track_id = t.id AND (v.source <> 'imported' OR v.verification <> 'pending')) AS touched
    FROM tracks t
    LEFT JOIN track_metadata m ON m.track_id = t.id
    CROSS JOIN metadata_settings s
) f
CROSS JOIN LATERAL (
    SELECT array_remove(ARRAY[
        CASE WHEN 'language' = ANY(f.required) AND NOT (f.has_language OR (f.language_unknown AND f.unknown_verification <> 'rejected')) THEN 'language' END,
        CASE WHEN 'genre' = ANY(f.required) AND NOT f.has_genre THEN 'genre' END,
        CASE WHEN 'culture' = ANY(f.required) AND NOT f.has_culture THEN 'culture' END,
        CASE WHEN 'region' = ANY(f.required) AND NOT f.has_region THEN 'region' END], NULL) AS missing
) x;
