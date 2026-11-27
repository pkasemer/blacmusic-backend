-- Timed lyrics may carry a translation under each line (kept with the line in `lines`). lrc is what was typed, so it
-- can be edited again without losing anything; empty for plain lyrics and for lyrics saved before this.
ALTER TABLE track_lyrics ADD COLUMN lrc text NOT NULL DEFAULT '';
