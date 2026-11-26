-- Automatic audio import: a transfer song can ask the server to fetch its audio from the
-- artist's own YouTube video (audio_source = 'youtube') or wait for a file the artist uploads
-- ('upload'). A fetch that fails falls back to 'upload' with the reason in fetch_error.
-- Songs that are not on YouTube at all are "manual" items (video_id 'manual:<n>').

ALTER TABLE transfer_items
    ADD COLUMN audio_source   text NOT NULL DEFAULT 'upload' CHECK (audio_source IN ('youtube', 'upload')),
    ADD COLUMN fetch_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN next_fetch_at  timestamptz,
    ADD COLUMN fetch_error    text NOT NULL DEFAULT '';

ALTER TABLE transfer_items DROP CONSTRAINT transfer_items_status_check;
ALTER TABLE transfer_items ADD CONSTRAINT transfer_items_status_check
    CHECK (status IN ('awaiting_audio', 'fetching', 'uploading', 'processing', 'cover', 'publishing', 'done', 'failed', 'skipped_duplicate', 'dropped'));

-- The artist's confirmation that they own the rights to what is imported, with the terms version.
ALTER TABLE transfers
    ADD COLUMN rights_accepted_at timestamptz,
    ADD COLUMN terms_version      text NOT NULL DEFAULT '';
