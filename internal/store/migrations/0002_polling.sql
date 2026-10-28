-- Per-repo poll bookkeeping. kind is 'release' or 'tag'.
CREATE TABLE poll_state (
    provider        TEXT    NOT NULL,
    repo            TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    etag            TEXT    NOT NULL DEFAULT '',
    -- 0 until the first successful poll has recorded the existing refs.
    baselined       INTEGER NOT NULL DEFAULT 0,
    last_polled_at  INTEGER,
    last_success_at INTEGER,
    last_error      TEXT,
    PRIMARY KEY (provider, repo, kind)
);

-- Every upstream release or tag Fork Keeper has seen, so each is acted on once.
CREATE TABLE upstream_refs (
    provider      TEXT    NOT NULL,
    repo          TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    title         TEXT,
    prerelease    INTEGER NOT NULL DEFAULT 0,
    published_at  INTEGER,
    url           TEXT,
    first_seen_at INTEGER NOT NULL,
    PRIMARY KEY (provider, repo, kind, name)
);
