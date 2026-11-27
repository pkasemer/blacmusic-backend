-- Featured artists are the featured_artist credits of a song and of its release. Studio artists can name anyone in
-- a credit, so a featured artist links to that artist's profile (with their name, picture and badge) only when the
-- team attached them. A credit the artist wrote shows as the name they typed.
ALTER TABLE release_credits ADD COLUMN by_team boolean NOT NULL DEFAULT false;

-- What a song shows, in the order saved: never the main artist, no banned artist, each artist or name once.
-- NULL when there are none.
CREATE FUNCTION featured_artists(song text, release text, main_artist text) RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT jsonb_agg(jsonb_build_object('artistId', x.artist_id, 'name', x.name, 'avatarId', x.avatar_id,
                     'imageUrl', x.image_url, 'verified', x.verified) ORDER BY x.position, x.created_at, x.id)
    FROM (
        SELECT DISTINCT ON (lower(e.name)) e.* FROM (
            SELECT c.id, c.position, c.created_at,
                   CASE WHEN c.by_team THEN a.id END AS artist_id,
                   CASE WHEN c.by_team AND a.id IS NOT NULL THEN a.name ELSE c.name END AS name,
                   CASE WHEN c.by_team THEN COALESCE(u.avatar_image_id, a.avatar_image_id) END AS avatar_id,
                   CASE WHEN c.by_team THEN a.image_url END AS image_url,
                   c.by_team AND COALESCE(a.verification = 'verified', false) AS verified
            FROM release_credits c
            LEFT JOIN artists a ON a.id = c.artist_id
            LEFT JOIN users u ON u.id = a.owner_user_id
            WHERE c.role = 'featured_artist' AND (c.track_id = song OR (c.track_id IS NULL AND c.release_id = release))
              AND a.banned_at IS NULL
              AND (c.artist_id IS NULL OR c.artist_id IS DISTINCT FROM main_artist)
        ) e
        WHERE lower(e.name) IS DISTINCT FROM (SELECT lower(m.name) FROM artists m WHERE m.id = main_artist)
        ORDER BY lower(e.name), (e.artist_id IS NULL), e.position, e.created_at, e.id
    ) x
$$;
