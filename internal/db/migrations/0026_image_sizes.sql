-- What a picture really takes: the bytes of the three stored sizes. (size_bytes is the size of the uploaded file,
-- which is deleted after processing.) NULL until measured; the worker measures pictures made before this column.
ALTER TABLE images ADD COLUMN stored_bytes bigint;
CREATE INDEX images_unmeasured_idx ON images (created_at) WHERE stored_bytes IS NULL AND status = 'ready';
