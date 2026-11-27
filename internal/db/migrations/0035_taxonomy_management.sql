-- Managing the catalogue's terms. The team can now add and rename genres, so the app's genre list comes from here.
ALTER TABLE tax_genres ADD COLUMN slug text; -- the id GET /v1/genres gives; it never changes
UPDATE tax_genres SET slug = CASE id
    WHEN 'hip_hop' THEN 'hip-hop-rap' WHEN 'kadongo_kamu' THEN 'kadongo-kamu' ELSE id END;
ALTER TABLE tax_genres ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX tax_genres_slug_idx ON tax_genres (slug);

-- A renamed genre's earlier names still work for older apps and import scripts, which are given the new one.
CREATE TABLE tax_genre_aliases (
    label    text PRIMARY KEY, -- lower case
    genre_id text NOT NULL REFERENCES tax_genres(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX tax_language_groups_label_idx ON tax_language_groups (lower(label));
CREATE UNIQUE INDEX tax_languages_label_idx ON tax_languages (lower(label));
CREATE UNIQUE INDEX tax_cultures_label_idx ON tax_cultures (lower(label));
CREATE UNIQUE INDEX tax_regions_label_idx ON tax_regions (lower(label));
