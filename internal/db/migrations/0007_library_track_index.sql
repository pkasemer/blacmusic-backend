-- Counting how many listeners saved a track (browse "popular") needs to look up library entries by track.
CREATE INDEX IF NOT EXISTS library_tracks_track_idx ON library_tracks (track_id);
