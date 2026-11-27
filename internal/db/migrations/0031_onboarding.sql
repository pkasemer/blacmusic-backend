-- What a listener told us while setting up the app. Languages they prefer and languages they understand are kept
-- apart. Only what was chosen is stored: nothing left unchosen counts against anything, and nothing about a person
-- (their ethnicity least of all) is worked out from these. Everything goes when the account is deleted.
CREATE TABLE listener_preferences (
    user_id              uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    preferred_languages  text[] NOT NULL DEFAULT '{}',
    understood_languages text[] NOT NULL DEFAULT '{}',
    regions              text[] NOT NULL DEFAULT '{}',
    discovery_mode       text NOT NULL DEFAULT '' CHECK (discovery_mode IN ('', 'familiar', 'explore_more', 'everything_uganda')),
    genres               text[] NOT NULL DEFAULT '{}',
    artist_ids           text[] NOT NULL DEFAULT '{}',
    completed_at         timestamptz,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- The start of a listener's recommendation profile, for a later model: what they had chosen when they finished
-- setting up. Later changes are read from listener_preferences.
CREATE TABLE recommendation_profiles (
    user_id        uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    seed           jsonb NOT NULL,
    initialised_at timestamptz NOT NULL DEFAULT now()
);
