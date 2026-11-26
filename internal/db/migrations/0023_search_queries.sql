-- What people search for, kept only in aggregate form: no user id and no address. One row per first-page search.
-- Used by the admin site's "Search insights"; rows older than 180 days are deleted by the worker.
CREATE TABLE search_queries (
    id      bigserial PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now(),
    kind    text NOT NULL CHECK (kind IN ('tracks', 'artists', 'playlists', 'albums')),
    query   text NOT NULL,
    results integer NOT NULL DEFAULT 0
);
CREATE INDEX search_queries_at_idx ON search_queries (at);
CREATE INDEX search_queries_query_idx ON search_queries (query, at);
