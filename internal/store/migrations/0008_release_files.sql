-- Release files (Windows and Linux builds) copied from an upstream release
-- to a release on the fork. Nothing is kept locally; sha256 is the digest
-- verified after upload. status is 'uploaded' or 'failed'.
CREATE TABLE release_files (
    provider        TEXT    NOT NULL,
    repo            TEXT    NOT NULL,
    tag             TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    platform        TEXT    NOT NULL,
    size            INTEGER NOT NULL,
    sha256          TEXT    NOT NULL DEFAULT '',
    fork            TEXT    NOT NULL,
    fork_release_id INTEGER NOT NULL DEFAULT 0,
    fork_asset_id   INTEGER NOT NULL DEFAULT 0,
    status          TEXT    NOT NULL,
    error           TEXT    NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (provider, repo, tag, name)
);
