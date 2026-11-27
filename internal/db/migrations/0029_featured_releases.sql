-- The albums and EPs the team features on the app's home screen. A featured release has a position; only the
-- order of the positions matters.
ALTER TABLE albums ADD COLUMN featured_position integer;
CREATE INDEX albums_featured_idx ON albums (featured_position) WHERE featured_position IS NOT NULL;
