-- Resumable (multipart) uploads and loudness measurements.

-- A multipart upload keeps the object store's upload id until it is completed
-- or aborted. part_size is what the client was told to use; the server checks
-- every part against it when the upload is completed.
ALTER TABLE uploads
    ADD COLUMN multipart_id text   NOT NULL DEFAULT '',
    ADD COLUMN part_size    bigint NOT NULL DEFAULT 0;

-- EBU R128 loudness of the original, so the app can match volume between tracks.
-- NULL means unknown (not measured yet, or the track is digital silence).
-- loudness_checked_at is set whenever a measurement was attempted, so a track
-- that cannot be measured is not retried forever.
ALTER TABLE tracks
    ADD COLUMN loudness_lufs       double precision,  -- integrated loudness, LUFS
    ADD COLUMN true_peak_db        double precision,  -- true peak, dBTP
    ADD COLUMN loudness_range_lu   double precision,  -- loudness range, LU
    ADD COLUMN loudness_checked_at timestamptz;
CREATE INDEX tracks_loudness_todo_idx ON tracks (created_at) WHERE loudness_checked_at IS NULL;
