-- License scan of one upstream ref, cached so each version is downloaded
-- and scanned once. report is license.Report as JSON.
CREATE TABLE license_reports (
    provider   TEXT    NOT NULL,
    repo       TEXT    NOT NULL,
    ref        TEXT    NOT NULL,
    report     TEXT    NOT NULL,
    scanned_at INTEGER NOT NULL,
    PRIMARY KEY (provider, repo, ref)
);

-- Result of comparing a new upstream ref with the previous one.
-- diff is license.Diff as JSON; NULL for the first version seen.
CREATE TABLE license_checks (
    provider     TEXT    NOT NULL,
    repo         TEXT    NOT NULL,
    ref          TEXT    NOT NULL,
    previous_ref TEXT,
    changed      INTEGER NOT NULL,
    summary      TEXT    NOT NULL,
    diff         TEXT,
    checked_at   INTEGER NOT NULL,
    PRIMARY KEY (provider, repo, ref)
);
