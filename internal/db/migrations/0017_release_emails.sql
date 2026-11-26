-- Emails to artists about their releases (published, audio processed), queued in an outbox that the
-- worker sends with retries, and a switch for them next to the push switches.

ALTER TABLE notification_settings ADD COLUMN release_emails boolean NOT NULL DEFAULT true;

-- status: pending -> sending -> sent | failed. The address is read from the user when the
-- message is sent, so a changed email address is honoured.
CREATE TABLE email_messages (
    id              bigserial PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject         text NOT NULL,
    body            text NOT NULL,
    dedupe_key      text,
    status          text NOT NULL DEFAULT 'pending',
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until     timestamptz,
    error           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
-- the same event never queues the same email twice
CREATE UNIQUE INDEX email_messages_dedupe_idx ON email_messages (dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX email_messages_pending_idx ON email_messages (next_attempt_at) WHERE status = 'pending';
CREATE INDEX email_messages_user_idx ON email_messages (user_id, created_at DESC);
