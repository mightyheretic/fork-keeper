-- Approvals: one per upstream ref whose license decision needs a human
-- (require_approval, or block_auto_merge, which needs an override).
-- context is a JSON snapshot of the decision at the time it was made.
-- applied is 0 while an approval's effect on the fork's PR (mark ready,
-- drop the labels) still has to be carried out.
CREATE TABLE approvals (
    id          INTEGER PRIMARY KEY,
    provider    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    ref         TEXT    NOT NULL,
    decision_id INTEGER NOT NULL REFERENCES license_decisions (id),
    kind        TEXT    NOT NULL, -- 'approval' or 'override'
    status      TEXT    NOT NULL, -- 'pending', 'approved' or 'rejected'
    context     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    notified_at INTEGER,
    reminders   INTEGER NOT NULL DEFAULT 0,
    resolved_at INTEGER,
    resolved_by TEXT    NOT NULL DEFAULT '',
    resolution  TEXT    NOT NULL DEFAULT '',
    applied     INTEGER NOT NULL DEFAULT 1,
    UNIQUE (provider, repo, ref)
);

CREATE INDEX approvals_pending ON approvals (status) WHERE status = 'pending';

-- Decisions recorded before approvals existed. Ones whose PR was already
-- merged or closed by hand are settled.
INSERT INTO approvals (provider, repo, ref, decision_id, kind, status, context, created_at)
SELECT d.provider, d.repo, d.ref, d.id,
       CASE WHEN EXISTS (SELECT 1 FROM json_each(d.pending) WHERE value = 'block_auto_merge')
            THEN 'override' ELSE 'approval' END,
       'pending',
       json_object('rule', d.rule, 'scope', d.scope, 'reason', d.reason, 'actions', json(d.actions),
                   'merge_policy', d.merge_policy, 'previous_ref', COALESCE(d.previous_ref, '')),
       d.created_at
FROM license_decisions d
WHERE d.ref NOT LIKE 'main@%'
  AND EXISTS (SELECT 1 FROM json_each(d.pending) WHERE value IN ('require_approval', 'block_auto_merge'))
  AND NOT EXISTS (SELECT 1 FROM syncs s WHERE s.provider = d.provider AND s.repo = d.repo AND s.ref = d.ref
                  AND s.status IN ('merged', 'closed'));

-- Audit log: every freeze, unfreeze, approval, override and rejection,
-- with who did it, why, and a JSON snapshot of the context.
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    provider    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    ref         TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL,
    actor       TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    detail      TEXT    NOT NULL DEFAULT '',
    decision_id INTEGER,
    approval_id INTEGER,
    context     TEXT    NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL
);

CREATE INDEX audit_log_repo ON audit_log (repo, created_at);

INSERT INTO audit_log (provider, repo, kind, actor, detail, created_at)
SELECT provider, repo, kind, actor, detail, created_at FROM repo_events;

-- Freezes still in place, made before the audit log existed.
INSERT INTO audit_log (provider, repo, ref, kind, actor, reason, detail, decision_id, context, created_at)
SELECT r.provider, r.repo, COALESCE(r.frozen_ref, ''), 'freeze', 'forkkeeper', COALESCE(r.frozen_reason, ''),
       'frozen at ' || COALESCE(NULLIF(r.frozen_ref, ''), 'no version'), r.frozen_decision,
       COALESCE((SELECT json_object('decision_id', d.id, 'ref', d.ref, 'previous_ref', COALESCE(d.previous_ref, ''),
                                    'rule', d.rule, 'scope', d.scope, 'reason', d.reason, 'actions', json(d.actions),
                                    'merge_policy', d.merge_policy)
                 FROM license_decisions d WHERE d.id = r.frozen_decision), '{}'),
       COALESCE(r.frozen_at, r.updated_at)
FROM repo_state r WHERE r.frozen = 1;

DROP TABLE repo_events;

-- Notification outbox. key dedupes (a retried event doesn't notify
-- twice); message is the JSON message; sent lists the channels that
-- already have it, so a retry only goes to the ones that failed.
CREATE TABLE notifications (
    id           INTEGER PRIMARY KEY,
    key          TEXT    NOT NULL UNIQUE,
    kind         TEXT    NOT NULL,
    provider     TEXT    NOT NULL DEFAULT '',
    repo         TEXT    NOT NULL DEFAULT '',
    ref          TEXT    NOT NULL DEFAULT '',
    decision_id  INTEGER,
    message      TEXT    NOT NULL,
    status       TEXT    NOT NULL DEFAULT 'pending', -- 'pending', 'sent' or 'failed'
    sent         TEXT    NOT NULL DEFAULT '[]',
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT    NOT NULL DEFAULT '',
    next_attempt INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    sent_at      INTEGER
);

CREATE INDEX notifications_due ON notifications (next_attempt) WHERE status = 'pending';
