-- One row per upstream ref the merge engine handled. A newer release
-- updates the same staging branch and PR, so older open rows for that PR
-- become 'superseded'. patches and conflicts are JSON arrays.
CREATE TABLE syncs (
    id             INTEGER PRIMARY KEY,
    provider       TEXT    NOT NULL,
    repo           TEXT    NOT NULL, -- upstream
    ref            TEXT    NOT NULL,
    fork           TEXT    NOT NULL,
    status         TEXT    NOT NULL,
    strategy       TEXT    NOT NULL DEFAULT '',
    merge_policy   TEXT    NOT NULL DEFAULT '',
    stable_branch  TEXT    NOT NULL DEFAULT '',
    staging_branch TEXT    NOT NULL DEFAULT '',
    head_sha       TEXT    NOT NULL DEFAULT '',
    pr_number      INTEGER,
    pr_url         TEXT    NOT NULL DEFAULT '',
    patches        TEXT    NOT NULL DEFAULT '[]',
    conflicts      TEXT    NOT NULL DEFAULT '[]',
    detail         TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (provider, repo, ref)
);

CREATE INDEX syncs_open ON syncs (status) WHERE status = 'pr_open';
