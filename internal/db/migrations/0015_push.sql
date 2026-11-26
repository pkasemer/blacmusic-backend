-- Push notifications through Expo's push service: the phones' push tokens, what each user
-- wants to be told about, and an outbox of messages (one row per message per device).

CREATE TABLE push_tokens (
    token       text PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform    text NOT NULL CHECK (platform IN ('ios', 'android')),
    device_name text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX push_tokens_user_idx ON push_tokens (user_id, updated_at DESC);

-- A missing row means the defaults: everything on.
CREATE TABLE notification_settings (
    user_id   uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    transfers boolean NOT NULL DEFAULT true,
    releases  boolean NOT NULL DEFAULT true
);

-- status: pending -> sending -> sent (Expo accepted it, receipt not read yet) -> delivered
--         | failed | invalid_token (the device is gone) | expired (no receipt within a day)
CREATE TABLE push_messages (
    id               bigserial PRIMARY KEY,
    user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token            text NOT NULL,
    title            text NOT NULL,
    body             text NOT NULL,
    data             jsonb NOT NULL DEFAULT '{}',
    dedupe_key       text,
    status           text NOT NULL DEFAULT 'pending',
    attempts         integer NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    lease_until      timestamptz,
    ticket_id        text,
    receipt_check_at timestamptz,
    error            text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    sent_at          timestamptz
);
-- the same event never queues the same message for the same device twice
CREATE UNIQUE INDEX push_messages_dedupe_idx ON push_messages (dedupe_key, token) WHERE dedupe_key IS NOT NULL;
CREATE INDEX push_messages_pending_idx ON push_messages (next_attempt_at) WHERE status = 'pending';
CREATE INDEX push_messages_receipt_idx ON push_messages (receipt_check_at) WHERE status = 'sent';
CREATE INDEX push_messages_user_idx ON push_messages (user_id, created_at DESC);
