-- Readings of the server, taken by the worker every few minutes and kept for 30 days, for the admin site's history.
CREATE TABLE server_samples (
    at                 timestamptz PRIMARY KEY DEFAULT now(),
    load1              real    NOT NULL,
    load5              real    NOT NULL,
    mem_total          bigint  NOT NULL,
    mem_available      bigint  NOT NULL,
    disk_total         bigint  NOT NULL,
    disk_used          bigint  NOT NULL,
    disk_free          bigint  NOT NULL,
    uploads_queued     integer NOT NULL,
    uploads_processing integer NOT NULL,
    -- what is really in storage, by the first part of the key; measured less often because it lists every object
    objects            jsonb
);
