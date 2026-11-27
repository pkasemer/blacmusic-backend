-- When a worker picked an upload up. locked_at is now the worker's regular "still working" signal, so this is what
-- says how long the job has been running, and which claim a worker holds (a job restarted from the dashboard gets
-- a new one, and the earlier worker must leave it alone).
ALTER TABLE uploads ADD COLUMN claimed_at timestamptz;
UPDATE uploads SET claimed_at = locked_at WHERE status = 'processing';
