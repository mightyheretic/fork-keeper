package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Audit entry kinds.
const (
	AuditFreeze   = "freeze"
	AuditUnfreeze = "unfreeze"
	AuditApprove  = "approve"
	AuditOverride = "override"
	AuditReject   = "reject"
)

// ActorForkKeeper is the actor for changes Fork Keeper makes on its own,
// following a license rule.
const ActorForkKeeper = "forkkeeper"

// AuditEntry is one license decision made by a person or a rule: a freeze,
// unfreeze, approval, override or rejection.
type AuditEntry struct {
	ID       int64
	Provider string
	Repo     string
	Ref      string
	Kind     string
	Actor    string
	Reason   string
	Detail   string
	// DecisionID and ApprovalID link the rule decision and approval, if any.
	DecisionID int64
	ApprovalID int64
	// Context is a JSON snapshot of what the decision was based on.
	Context   json.RawMessage
	CreatedAt time.Time
}

func addAudit(ctx context.Context, db execer, e AuditEntry, now int64) error {
	ctxJSON := string(e.Context)
	if ctxJSON == "" {
		ctxJSON = "{}"
	}
	nullID := func(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: id != 0} }
	_, err := db.ExecContext(ctx, `
		INSERT INTO audit_log (provider, repo, ref, kind, actor, reason, detail, decision_id, approval_id, context, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Provider, e.Repo, e.Ref, e.Kind, e.Actor, e.Reason, e.Detail, nullID(e.DecisionID), nullID(e.ApprovalID),
		ctxJSON, now)
	if err != nil {
		return fmt.Errorf("record audit entry: %w", err)
	}
	return nil
}

// AuditLog returns recent entries for repo ("" for all), newest first.
func (s *Store) AuditLog(ctx context.Context, repo string, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider, repo, ref, kind, actor, reason, detail, COALESCE(decision_id, 0),
			COALESCE(approval_id, 0), context, created_at
		FROM audit_log WHERE ? = '' OR repo = ? COLLATE NOCASE
		ORDER BY created_at DESC, id DESC LIMIT ?`, repo, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ctxJSON string
		var at int64
		if err := rows.Scan(&e.ID, &e.Provider, &e.Repo, &e.Ref, &e.Kind, &e.Actor, &e.Reason, &e.Detail,
			&e.DecisionID, &e.ApprovalID, &ctxJSON, &at); err != nil {
			return nil, err
		}
		e.Context = json.RawMessage(ctxJSON)
		e.CreatedAt = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// decisionContext snapshots a decision for the audit log and approvals.
func decisionContext(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, decisionID int64) (json.RawMessage, error) {
	if decisionID == 0 {
		return json.RawMessage("{}"), nil
	}
	var raw string
	err := q.QueryRowContext(ctx, `
		SELECT json_object('decision_id', id, 'ref', ref, 'previous_ref', COALESCE(previous_ref, ''),
			'rule', rule, 'scope', scope, 'reason', reason, 'actions', json(actions), 'merge_policy', merge_policy)
		FROM license_decisions WHERE id = ?`, decisionID).Scan(&raw)
	if err == sql.ErrNoRows {
		return json.RawMessage("{}"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot decision: %w", err)
	}
	return json.RawMessage(raw), nil
}
