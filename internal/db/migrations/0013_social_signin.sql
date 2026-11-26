-- Sign in with Apple / Google. An identity links a provider's stable user id to
-- one of our accounts; the account itself (users) is the same as for email.

CREATE TABLE auth_identities (
    provider   text NOT NULL CHECK (provider IN ('apple', 'google')),
    subject    text NOT NULL,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);
CREATE INDEX auth_identities_user_idx ON auth_identities (user_id);

-- A started Google sign-in: single use, ten minutes. The PKCE verifier stays here.
CREATE TABLE auth_google_states (
    state      text PRIMARY KEY,
    verifier   text NOT NULL,
    expires_at timestamptz NOT NULL
);

-- What the in-app browser hands back to the app after Google: a one-time code, valid
-- 60 seconds, that the app redeems for a normal login. Only its hash is stored.
CREATE TABLE auth_login_codes (
    code_hash  text PRIMARY KEY,
    claims     jsonb NOT NULL,
    expires_at timestamptz NOT NULL
);
