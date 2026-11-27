-- Catalogue metadata: what the team has said about each song, one row per song and term, each with where it came
-- from and whether it has been verified. Nothing is guessed: no language from an artist or region, no culture from a
-- language. Terms are switched off (active = false), not deleted, once songs use them. Term ids are permanent.
CREATE TABLE tax_language_groups (id text PRIMARY KEY, label text NOT NULL, position integer NOT NULL DEFAULT 0);
CREATE TABLE tax_languages (id text PRIMARY KEY, label text NOT NULL, group_id text REFERENCES tax_language_groups(id),
    active boolean NOT NULL DEFAULT true, position integer NOT NULL DEFAULT 0);
CREATE TABLE tax_cultures (id text PRIMARY KEY, label text NOT NULL, active boolean NOT NULL DEFAULT true, position integer NOT NULL DEFAULT 0);
CREATE TABLE tax_regions (id text PRIMARY KEY, label text NOT NULL, active boolean NOT NULL DEFAULT true, position integer NOT NULL DEFAULT 0);
-- A genre's label is what tracks.genre holds for the songs that have it as their main genre.
CREATE TABLE tax_genres (id text PRIMARY KEY, label text NOT NULL, parent_id text REFERENCES tax_genres(id),
    active boolean NOT NULL DEFAULT true, position integer NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX tax_genres_label_idx ON tax_genres (lower(label));
CREATE TABLE tax_attributes (kind text NOT NULL CHECK (kind IN ('mood', 'energy', 'danceability', 'instrumentation')),
    id text NOT NULL, label text NOT NULL, active boolean NOT NULL DEFAULT true, position integer NOT NULL DEFAULT 0, PRIMARY KEY (kind, id));

INSERT INTO tax_language_groups (id, label, position) VALUES
  ('runyakitara', 'Runyakitara', 1),
  ('central_eastern_bantu', 'Central and Eastern Bantu', 2),
  ('luo', 'Luo / Lwo', 3),
  ('ateker', 'Ateker', 4),
  ('moru_madi', 'Moru–Ma''di', 5),
  ('national_international', 'National and International', 6);
INSERT INTO tax_languages (id, label, group_id, position) VALUES
  ('runyankole', 'Runyankole', 'runyakitara', 1),
  ('rukiga', 'Rukiga', 'runyakitara', 2),
  ('runyoro', 'Runyoro', 'runyakitara', 3),
  ('rutooro', 'Rutooro', 'runyakitara', 4),
  ('luganda', 'Luganda', 'central_eastern_bantu', 5),
  ('lusoga', 'Lusoga', 'central_eastern_bantu', 6),
  ('lugwere', 'Lugwere', 'central_eastern_bantu', 7),
  ('luruuli', 'Luruuli', 'central_eastern_bantu', 8),
  ('lussese', 'Lussese', 'central_eastern_bantu', 9),
  ('acholi', 'Acholi', 'luo', 10),
  ('lango', 'Lango', 'luo', 11),
  ('alur', 'Alur', 'luo', 12),
  ('kumam', 'Kumam', 'luo', 13),
  ('adhola', 'Adhola (Jopadhola)', 'luo', 14),
  ('ateso', 'Ateso', 'ateker', 15),
  ('ngakarimojong', 'Ngakarimojong', 'ateker', 16),
  ('jie', 'Jie', 'ateker', 17),
  ('dodoth', 'Dodoth', 'ateker', 18),
  ('pokot', 'Pokot', 'ateker', 19),
  ('lugbara', 'Lugbara', 'moru_madi', 20),
  ('aringa', 'Aringa', 'moru_madi', 21),
  ('madi', 'Ma''di', 'moru_madi', 22),
  ('english', 'English', 'national_international', 23),
  ('swahili', 'Swahili', 'national_international', 24);
INSERT INTO tax_cultures (id, label, position) VALUES
  ('acholi', 'Acholi', 1),
  ('lango', 'Lango', 2),
  ('baganda', 'Baganda', 3),
  ('basoga', 'Basoga', 4),
  ('banyankole', 'Banyankole', 5),
  ('bakiga', 'Bakiga', 6),
  ('banyoro', 'Banyoro', 7),
  ('batooro', 'Batooro', 8),
  ('iteso', 'Iteso', 9),
  ('karamojong', 'Karamojong', 10),
  ('alur', 'Alur', 11),
  ('lugbara', 'Lugbara', 12),
  ('madi', 'Ma''di', 13);
INSERT INTO tax_regions (id, label, position) VALUES
  ('western_uganda', 'Western Uganda', 1),
  ('central_uganda', 'Central Uganda', 2),
  ('northern_uganda', 'Northern Uganda', 3),
  ('eastern_uganda', 'Eastern Uganda', 4),
  ('karamoja', 'Karamoja', 5),
  ('west_nile', 'West Nile', 6);
INSERT INTO tax_genres (id, label, position) VALUES
  ('pop', 'Pop', 1),
  ('hip_hop', 'Hip-Hop/Rap', 2),
  ('rap', 'Rap', 3),
  ('afrobeats', 'Afrobeats', 4),
  ('afrobeat', 'Afrobeat', 5),
  ('rnb-soul', 'R&B/Soul', 6),
  ('rnb', 'R&B', 7),
  ('amapiano', 'Amapiano', 8),
  ('dancehall', 'Dancehall', 9),
  ('gospel', 'Gospel', 10),
  ('rock', 'Rock', 11),
  ('electronic-dance', 'Electronic/Dance', 12),
  ('dance', 'Dance', 13),
  ('house', 'House', 14),
  ('reggae', 'Reggae', 15),
  ('latin', 'Latin', 16),
  ('bongo-flava', 'Bongo Flava', 17),
  ('kidandali', 'Kidandali', 18),
  ('afro-house', 'Afro House', 19),
  ('trap', 'Trap', 20),
  ('afropop', 'Afropop', 21),
  ('alternative', 'Alternative', 22),
  ('indie', 'Indie', 23),
  ('country', 'Country', 24),
  ('jazz', 'Jazz', 25),
  ('blues', 'Blues', 26),
  ('classical', 'Classical', 27),
  ('folk', 'Folk', 28),
  ('metal', 'Metal', 29),
  ('punk', 'Punk', 30),
  ('k-pop', 'K-Pop', 31),
  ('funk', 'Funk', 32),
  ('disco', 'Disco', 33),
  ('techno', 'Techno', 34),
  ('drill', 'Drill', 35),
  ('soukous', 'Soukous', 36),
  ('zouk', 'Zouk', 37),
  ('highlife', 'Highlife', 38),
  ('gengetone', 'Gengetone', 39),
  ('gqom', 'Gqom', 40),
  ('kwaito', 'Kwaito', 41),
  ('taarab', 'Taarab', 42),
  ('lo-fi', 'Lo-fi', 43),
  ('ambient', 'Ambient', 44),
  ('kadongo_kamu', 'Kadongo Kamu', 45),
  ('traditional', 'Traditional', 46),
  ('other', 'Other', 47);
INSERT INTO tax_attributes (kind, id, label, position) VALUES
  ('mood', 'happy', 'Happy', 1),
  ('mood', 'sad', 'Sad', 2),
  ('mood', 'romantic', 'Romantic', 3),
  ('mood', 'energetic', 'Energetic', 4),
  ('mood', 'inspirational', 'Inspirational', 5),
  ('mood', 'spiritual', 'Spiritual', 6),
  ('mood', 'relaxed', 'Relaxed', 7),
  ('mood', 'aggressive', 'Aggressive', 8),
  ('mood', 'nostalgic', 'Nostalgic', 9),
  ('energy', 'low', 'Low', 1),
  ('energy', 'medium', 'Medium', 2),
  ('energy', 'high', 'High', 3),
  ('danceability', 'low', 'Low', 1),
  ('danceability', 'medium', 'Medium', 2),
  ('danceability', 'high', 'High', 3),
  ('instrumentation', 'acoustic', 'Acoustic', 1),
  ('instrumentation', 'electronic', 'Electronic', 2),
  ('instrumentation', 'percussion', 'Percussion', 3),
  ('instrumentation', 'guitar', 'Guitar', 4),
  ('instrumentation', 'piano', 'Piano', 5),
  ('instrumentation', 'traditional_instruments', 'Traditional Instruments', 6),
  ('instrumentation', 'brass', 'Brass', 7),
  ('instrumentation', 'strings', 'Strings', 8);

CREATE TABLE track_languages (
    track_id     text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    language_id  text NOT NULL REFERENCES tax_languages(id),
    role         text NOT NULL CHECK (role IN ('primary', 'secondary')),
    source       text NOT NULL DEFAULT 'admin' CHECK (source IN ('artist_submission', 'admin', 'ai_suggestion', 'imported')),
    verification text NOT NULL DEFAULT 'pending' CHECK (verification IN ('pending', 'verified', 'rejected')),
    confidence   real CHECK (confidence BETWEEN 0 AND 1),
    verified_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    verified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (track_id, language_id)
);
CREATE UNIQUE INDEX track_languages_one_primary ON track_languages (track_id) WHERE role = 'primary';
CREATE TABLE track_cultures (
    track_id     text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    culture_id   text NOT NULL REFERENCES tax_cultures(id),
    source       text NOT NULL DEFAULT 'admin' CHECK (source IN ('artist_submission', 'admin', 'ai_suggestion', 'imported')),
    verification text NOT NULL DEFAULT 'pending' CHECK (verification IN ('pending', 'verified', 'rejected')),
    confidence   real CHECK (confidence BETWEEN 0 AND 1),
    verified_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    verified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (track_id, culture_id)
);
CREATE TABLE track_regions (
    track_id     text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    region_id    text NOT NULL REFERENCES tax_regions(id),
    source       text NOT NULL DEFAULT 'admin' CHECK (source IN ('artist_submission', 'admin', 'ai_suggestion', 'imported')),
    verification text NOT NULL DEFAULT 'pending' CHECK (verification IN ('pending', 'verified', 'rejected')),
    confidence   real CHECK (confidence BETWEEN 0 AND 1),
    verified_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    verified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (track_id, region_id)
);
CREATE TABLE track_genres (
    track_id     text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    genre_id     text NOT NULL REFERENCES tax_genres(id),
    is_primary   boolean NOT NULL DEFAULT false,
    source       text NOT NULL DEFAULT 'admin' CHECK (source IN ('artist_submission', 'admin', 'ai_suggestion', 'imported')),
    verification text NOT NULL DEFAULT 'pending' CHECK (verification IN ('pending', 'verified', 'rejected')),
    confidence   real CHECK (confidence BETWEEN 0 AND 1),
    verified_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    verified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (track_id, genre_id)
);
CREATE UNIQUE INDEX track_genres_one_primary ON track_genres (track_id) WHERE is_primary;
CREATE TABLE track_attributes (
    track_id     text NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    kind         text NOT NULL,
    attribute_id text NOT NULL,
    source       text NOT NULL DEFAULT 'admin' CHECK (source IN ('artist_submission', 'admin', 'ai_suggestion', 'imported')),
    verification text NOT NULL DEFAULT 'pending' CHECK (verification IN ('pending', 'verified', 'rejected')),
    confidence   real CHECK (confidence BETWEEN 0 AND 1),
    verified_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    verified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (track_id, kind, attribute_id),
    FOREIGN KEY (kind, attribute_id) REFERENCES tax_attributes(kind, id)
);
CREATE UNIQUE INDEX track_attributes_single ON track_attributes (track_id, kind) WHERE kind IN ('energy', 'danceability');
CREATE INDEX track_languages_term_idx ON track_languages (language_id);
CREATE INDEX track_genres_term_idx ON track_genres (genre_id);

-- The song's metadata as a whole. language_unknown says the team looked and does not know the language: no language
-- rows on their own mean nothing. version changes with every change, so two editors cannot overwrite each other.
CREATE TABLE track_metadata (
    track_id         text PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    language_unknown boolean NOT NULL DEFAULT false,
    status           text NOT NULL DEFAULT 'not_started' CHECK (status IN ('not_started', 'in_progress', 'pending_review', 'verified')),
    version          bigint NOT NULL DEFAULT 0,
    updated_at       timestamptz,
    updated_by       uuid REFERENCES users(id) ON DELETE SET NULL
);

-- tracks.genre stays what it was (uploads, edits and imports write it); the main genre here follows it.
CREATE FUNCTION sync_track_genre() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE wanted text;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.genre IS NOT DISTINCT FROM OLD.genre THEN RETURN NULL; END IF;
    SELECT id INTO wanted FROM tax_genres WHERE lower(label) = lower(NEW.genre);
    IF EXISTS (SELECT 1 FROM track_genres WHERE track_id = NEW.id AND is_primary AND genre_id = wanted) THEN RETURN NULL; END IF;
    DELETE FROM track_genres WHERE track_id = NEW.id AND is_primary AND source = 'imported';
    UPDATE track_genres SET is_primary = false, verification = 'pending', verified_by = NULL, verified_at = NULL
        WHERE track_id = NEW.id AND is_primary;
    IF wanted IS NOT NULL THEN
        INSERT INTO track_genres (track_id, genre_id, is_primary, source) VALUES (NEW.id, wanted, true, 'imported')
        ON CONFLICT (track_id, genre_id) DO UPDATE SET is_primary = true, verification = 'pending', verified_by = NULL, verified_at = NULL;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        INSERT INTO track_metadata (track_id, version) VALUES (NEW.id, 1)
        ON CONFLICT (track_id) DO UPDATE SET version = track_metadata.version + 1;
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER tracks_genre_sync AFTER INSERT OR UPDATE OF genre ON tracks FOR EACH ROW EXECUTE FUNCTION sync_track_genre();

-- the genres songs already have, as unverified imports
INSERT INTO track_genres (track_id, genre_id, is_primary, source)
SELECT t.id, g.id, true, 'imported' FROM tracks t JOIN tax_genres g ON lower(g.label) = lower(t.genre);
