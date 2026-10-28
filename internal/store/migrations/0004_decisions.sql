-- Rule engine decisions, one per upstream ref (re-evaluation replaces it).
-- actions, matched and pending are JSON arrays. pending lists actions that
-- later stages (merge engine, archiver, notifier) still have to carry out.
CREATE TABLE license_decisions (
    id           INTEGER PRIMARY KEY,
    provider     TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    ref          TEXT    NOT NULL,
    previous_ref TEXT,
    rule         TEXT    NOT NULL,
    scope        TEXT    NOT NULL,
    actions      TEXT    NOT NULL,
    merge_policy TEXT    NOT NULL,
    reason       TEXT    NOT NULL,
    matched      TEXT    NOT NULL,
    pending      TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    UNIQUE (provider, repo, ref)
);

-- Per-repo state. A frozen repo stays at frozen_ref (the last version that
-- passed the license gate) until someone unfreezes it.
CREATE TABLE repo_state (
    provider        TEXT    NOT NULL,
    repo            TEXT    NOT NULL,
    frozen          INTEGER NOT NULL DEFAULT 0,
    frozen_ref      TEXT,
    frozen_at       INTEGER,
    frozen_reason   TEXT,
    frozen_decision INTEGER REFERENCES license_decisions (id),
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (provider, repo)
);

-- Manual changes to repo state (for now: unfreezes), with who and why.
CREATE TABLE repo_events (
    id         INTEGER PRIMARY KEY,
    provider   TEXT    NOT NULL,
    repo       TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    detail     TEXT    NOT NULL,
    actor      TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
