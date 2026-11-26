-- The home screen ranks artists, albums and tracks by how many listeners saved
-- or followed them. Those counts look entries up by the entity, not by the user.
CREATE INDEX IF NOT EXISTS library_artists_artist_idx ON library_artists (artist_id);
CREATE INDEX IF NOT EXISTS library_albums_album_idx ON library_albums (album_id);
