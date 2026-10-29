package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Approval kinds and statuses.
const (
	// ApprovalNeeded is for require_approval: Fork Keeper merges once
	// someone approves.
	ApprovalNeeded = "approval"
	// ApprovalOverride is for block_auto_merge: approving overrides the block.
	ApprovalOverride = "override"

	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
)

// ErrNotPending is returned when resolving an approval that is already
// resolved.
var ErrNotPending = errors.New("approval is not pending")

// Approval is a persistent request for a human decision on one upstream
// version.
type Approval struct {
	ID int64
	RefKey
	DecisionID int64
	Kind       string
	Status     string
	// Context is a JSON snapshot of the decision (rule, reason, actions,
	// previous version).
	Context    json.RawMessage
	CreatedAt  time.Time
	NotifiedAt time.Time
	Reminders  int
	ResolvedAt time.Time
	ResolvedBy string
	Resolution string
	// Applied is false while an approval's effect on the fork's PR is
	// still to be carried out.
	Applied bool
}

const approvalColumns = `id, provider, repo, ref, decision_id, kind, status, context, created_at,
	COALESCE(notified_at, 0), reminders, COALESCE(resolved_at, 0), resolved_by, resolution, applied`

func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var ctxJSON string
	var created, notified, resolved int64
	var applied int
	if err := row.Scan(&a.ID, &a.Provider, &a.Repo, &a.Ref, &a.DecisionID, &a.Kind, &a.Status, &ctxJSON, &created,
		&notified, &a.Reminders, &resolved, &a.ResolvedBy, &a.Resolution, &applied); err != nil {
		return a, err
	}
	a.Context = json.RawMessage(ctxJSON)
	a.CreatedAt = time.UnixMilli(created)
	if notified != 0 {
		a.NotifiedAt = time.UnixMilli(notified)
	}
	if resolved != 0 {
		a.ResolvedAt = time.UnixMilli(resolved)
	}
	a.Applied = applied == 1
	return a, nil
}

// SaveApproval records that the decision for key needs a human. A pending
// approval is updated to the latest decision; a resolved one is kept as it
// is, so re-checking a version doesn't ask again.
func (s *Store) SaveApproval(ctx context.Context, key RefKey, decisionID int64, kind string) (Approval, error) {
	var a Approval
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		snap, err := decisionContext(ctx, tx, decisionID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approvals (provider, repo, ref, decision_id, kind, status, context, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider, repo, ref) DO UPDATE SET
				decision_id = excluded.decision_id, kind = excluded.kind, context = excluded.context
			WHERE approvals.status = 'pending'`,
			key.Provider, key.Repo, key.Ref, decisionID, kind, ApprovalPending, string(snap), s.nowMillis()); err != nil {
			return fmt.Errorf("save approval: %w", err)
		}
		a, err = scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals
			WHERE provider = ? AND repo = ? AND ref = ?`, key.Provider, key.Repo, key.Ref))
		return err
	})
	return a, err
}

// Approval returns one approval by ID.
func (s *Store) Approval(ctx context.Context, id int64) (Approval, bool, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, fmt.Errorf("read approval: %w", err)
	}
	return a, true, nil
}

// ApprovalFor returns the approval for one upstream version.
func (s *Store) ApprovalFor(ctx context.Context, key RefKey) (Approval, bool, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE provider = ? AND repo = ? AND ref = ?`, key.Provider, key.Repo, key.Ref))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, fmt.Errorf("read approval: %w", err)
	}
	return a, true, nil
}

// Approvals lists approvals for repo ("" for all) with status ("" for
// any), newest first.
func (s *Store) Approvals(ctx context.Context, repo, status string, limit int) ([]Approval, error) {
	return s.listApprovals(ctx, `WHERE (? = '' OR repo = ? COLLATE NOCASE) AND (? = '' OR status = ?)
		ORDER BY created_at DESC, id DESC LIMIT ?`, repo, repo, status, status, limit)
}

// PendingApprovals lists the pending approvals of one upstream, oldest first.
func (s *Store) PendingApprovals(ctx context.Context, provider, repo string) ([]Approval, error) {
	return s.listApprovals(ctx, `WHERE provider = ? AND repo = ? AND status = ? ORDER BY created_at, id`,
		provider, repo, ApprovalPending)
}

// UnappliedApprovals lists approved approvals whose effect on the fork's
// PR hasn't been carried out yet.
func (s *Store) UnappliedApprovals(ctx context.Context) ([]Approval, error) {
	return s.listApprovals(ctx, `WHERE status = ? AND applied = 0 ORDER BY id`, ApprovalApproved)
}

// DueReminders lists pending approvals last notified (or created) before t.
func (s *Store) DueReminders(ctx context.Context, before time.Time) ([]Approval, error) {
	return s.listApprovals(ctx, `WHERE status = ? AND COALESCE(notified_at, created_at) < ? ORDER BY id`,
		ApprovalPending, before.UnixMilli())
}

func (s *Store) listApprovals(ctx context.Context, where string, args ...any) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Resolution is a human decision on an approval.
type Resolution struct {
	Approve bool
	Actor   string
	Reason  string
	// FreezeAt freezes the repo at this version on a rejection, so later
	// releases (which carry the rejected change) don't sync either.
	FreezeAt string
}

// ResolveApproval approves or rejects a pending approval, records it in
// the audit log and settles the decision's approval actions. A rejection
// also freezes the repo. It returns ErrNotPending if the approval was
// already resolved.
func (s *Store) ResolveApproval(ctx context.Context, id int64, r Resolution) (Approval, error) {
	var a Approval
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		a, err = scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no approval #%d", id)
		}
		if err != nil {
			return err
		}
		if a.Status != ApprovalPending {
			return fmt.Errorf("approval #%d was already %s by %s: %w", id, a.Status, a.ResolvedBy, ErrNotPending)
		}
		now := s.nowMillis()
		status, kind, applied := ApprovalRejected, AuditReject, 1
		if r.Approve {
			status, kind, applied = ApprovalApproved, AuditApprove, 0
			if a.Kind == ApprovalOverride {
				kind = AuditOverride
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, resolved_at = ?, resolved_by = ?,
			resolution = ?, applied = ? WHERE id = ?`, status, now, r.Actor, r.Reason, applied, id); err != nil {
			return err
		}
		a.Status, a.ResolvedAt, a.ResolvedBy, a.Resolution, a.Applied = status, time.UnixMilli(now), r.Actor, r.Reason, applied == 1
		detail := fmt.Sprintf("%s %s (approval #%d)", status, a.Ref, id)
		if a.Kind == ApprovalOverride && r.Approve {
			detail = fmt.Sprintf("overrode block_auto_merge on %s (approval #%d)", a.Ref, id)
		}
		if err := addAudit(ctx, tx, AuditEntry{Provider: a.Provider, Repo: a.Repo, Ref: a.Ref, Kind: kind, Actor: r.Actor,
			Reason: r.Reason, Detail: detail, DecisionID: a.DecisionID, ApprovalID: id, Context: a.Context}, now); err != nil {
			return err
		}
		if err := completePendingTx(ctx, tx, a.DecisionID, "require_approval", "block_auto_merge"); err != nil {
			return err
		}
		if !r.Approve {
			_, err := s.freezeTx(ctx, tx, a.Provider, a.Repo, r.FreezeAt, r.Actor,
				fmt.Sprintf("%s rejected: %s", a.Ref, r.Reason), a.DecisionID, id)
			return err
		}
		return nil
	})
	return a, err
}

// MarkApprovalApplied records that an approval's effect on the fork's PR
// was carried out.
func (s *Store) MarkApprovalApplied(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE approvals SET applied = 1 WHERE id = ?`, id)
	return err
}

// MarkApprovalNotified records a notification (or reminder) about a
// pending approval.
func (s *Store) MarkApprovalNotified(ctx context.Context, id int64, reminder bool) error {
	inc := 0
	if reminder {
		inc = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE approvals SET notified_at = ?, reminders = reminders + ? WHERE id = ?`,
		s.nowMillis(), inc, id)
	return err
}

// SetSyncPolicy changes the merge policy recorded for a sync, after an
// approval.
func (s *Store) SetSyncPolicy(ctx context.Context, id int64, policy, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE syncs SET merge_policy = ?, detail = ?, updated_at = ? WHERE id = ?`,
		policy, detail, s.nowMillis(), id)
	if err != nil {
		return fmt.Errorf("update sync policy: %w", err)
	}
	return nil
}

// OpenSyncFor returns the open sync PR record of an upstream on fork, if any.
func (s *Store) OpenSyncFor(ctx context.Context, provider, repo string) (Sync, bool, error) {
	recs, err := s.listSyncs(ctx, `WHERE provider = ? AND repo = ? AND status = ? ORDER BY updated_at DESC, id DESC LIMIT 1`,
		provider, repo, SyncPROpen)
	if err != nil || len(recs) == 0 {
		return Sync{}, false, err
	}
	return recs[0], true, nil
}
