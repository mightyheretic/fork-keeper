-- Source backups: every ref copied into the local archive repository.
-- kind is 'release', 'last-good' (the version before a license change) or
-- 'last-open' (the last default-branch commit before one).
CREATE TABLE archived_refs (
    provider   TEXT    NOT NULL,
    repo       TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    commit_sha TEXT    NOT NULL,
    reason     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (provider, repo, name)
);

-- Last push of each repo's backups to each destination (fork or mirror).
CREATE TABLE archive_pushes (
    provider    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    destination TEXT    NOT NULL,
    ok          INTEGER NOT NULL,
    refs        INTEGER NOT NULL,
    error       TEXT    NOT NULL DEFAULT '',
    pushed_at   INTEGER NOT NULL,
    PRIMARY KEY (provider, repo, destination)
);

-- Software Heritage "save code now" requests.
CREATE TABLE swh_requests (
    id           INTEGER PRIMARY KEY,
    provider     TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    origin_url   TEXT    NOT NULL,
    request_id   INTEGER,
    status       TEXT    NOT NULL,
    task_status  TEXT    NOT NULL DEFAULT '',
    reason       TEXT    NOT NULL DEFAULT '',
    requested_at INTEGER NOT NULL
);
CREATE INDEX swh_requests_repo ON swh_requests (provider, repo, requested_at);

-- Default-branch early warning: the last head checked and its top-level
-- license. report is the top-level license.Report as JSON.
CREATE TABLE main_watch (
    provider    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    etag        TEXT    NOT NULL DEFAULT '',
    head_sha    TEXT    NOT NULL DEFAULT '',
    fingerprint TEXT    NOT NULL DEFAULT '',
    report      TEXT    NOT NULL DEFAULT '',
    base_ref    TEXT    NOT NULL DEFAULT '',
    checked_at  INTEGER,
    last_error  TEXT,
    PRIMARY KEY (provider, repo)
);
