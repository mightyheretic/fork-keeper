package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Decision is a stored rule engine decision.
type Decision struct {
	ID int64
	RefKey
	PreviousRef string
	Rule        string
	Scope       string
	Actions     []string
	MergePolicy string
	Reason      string
	Matched     []string
	// Pending lists actions not carried out yet.
	Pending   []string
	CreatedAt time.Time
}

// SaveDecision records d, replacing any earlier decision for the same ref,
// and returns its ID.
func (s *Store) SaveDecision(ctx context.Context, d Decision) (int64, error) {
	enc := func(v []string) string {
		if v == nil {
			v = []string{}
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	var prev sql.NullString
	if d.PreviousRef != "" {
		prev = sql.NullString{String: d.PreviousRef, Valid: true}
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO license_decisions
			(provider, repo, ref, previous_ref, rule, scope, actions, merge_policy, reason, matched, pending, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, ref) DO UPDATE SET
			previous_ref = excluded.previous_ref, rule = excluded.rule, scope = excluded.scope,
			actions = excluded.actions, merge_policy = excluded.merge_policy, reason = excluded.reason,
			matched = excluded.matched, pending = excluded.pending, created_at = excluded.created_at
		RETURNING id`,
		d.Provider, d.Repo, d.Ref, prev, d.Rule, d.Scope, enc(d.Actions), d.MergePolicy, d.Reason,
		enc(d.Matched), enc(d.Pending), s.nowMillis()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("save decision: %w", err)
	}
	return id, nil
}

// Decisions returns recent decisions for repo ("" for all), newest first.
func (s *Store) Decisions(ctx context.Context, repo string, limit int) ([]Decision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider, repo, ref, COALESCE(previous_ref, ''), rule, scope, actions, merge_policy,
			reason, matched, pending, created_at
		FROM license_decisions WHERE ? = '' OR repo = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, repo, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var actions, matched, pending string
		var at int64
		if err := rows.Scan(&d.ID, &d.Provider, &d.Repo, &d.Ref, &d.PreviousRef, &d.Rule, &d.Scope,
			&actions, &d.MergePolicy, &d.Reason, &matched, &pending, &at); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(actions), &d.Actions)
		json.Unmarshal([]byte(matched), &d.Matched)
		json.Unmarshal([]byte(pending), &d.Pending)
		d.CreatedAt = time.UnixMilli(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RepoState is the persisted state of one upstream.
type RepoState struct {
	Provider string
	Repo     string
	Frozen   bool
	// FrozenRef is the last version that passed the license gate.
	FrozenRef      string
	FrozenAt       time.Time
	FrozenReason   string
	FrozenDecision int64
}

// RepoState returns the state of repo; the zero state if none is recorded.
func (s *Store) RepoState(ctx context.Context, provider, repo string) (RepoState, error) {
	st := RepoState{Provider: provider, Repo: repo}
	var frozen int
	var ref, reason sql.NullString
	var at, decision sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT frozen, frozen_ref, frozen_at, frozen_reason, frozen_decision FROM repo_state
		WHERE provider = ? AND repo = ?`, provider, repo).Scan(&frozen, &ref, &at, &reason, &decision)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read repo state: %w", err)
	}
	st.Frozen = frozen == 1
	st.FrozenRef, st.FrozenReason, st.FrozenDecision = ref.String, reason.String, decision.Int64
	if at.Valid {
		st.FrozenAt = time.UnixMilli(at.Int64)
	}
	return st, nil
}

// FrozenRepos returns every frozen repo.
func (s *Store) FrozenRepos(ctx context.Context) ([]RepoState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, repo FROM repo_state WHERE frozen = 1 ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("list frozen repos: %w", err)
	}
	var keys [][2]string
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	out := make([]RepoState, 0, len(keys))
	for _, k := range keys {
		st, err := s.RepoState(ctx, k[0], k[1])
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// Freeze freezes repo at ref, following a license rule. If it is already
// frozen, the original freeze point is kept and Freeze returns false.
func (s *Store) Freeze(ctx context.Context, provider, repo, ref, reason string, decisionID int64) (bool, error) {
	return s.freeze(ctx, provider, repo, ref, ActorForkKeeper, reason, decisionID, 0)
}

// FreezeBy freezes repo at ref by hand (or on a rejected approval, with
// approvalID set) and records who did it and why.
func (s *Store) FreezeBy(ctx context.Context, provider, repo, ref, actor, reason string, decisionID, approvalID int64) (bool, error) {
	return s.freeze(ctx, provider, repo, ref, actor, reason, decisionID, approvalID)
}

func (s *Store) freeze(ctx context.Context, provider, repo, ref, actor, reason string, decisionID, approvalID int64) (bool, error) {
	var frozen bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		frozen, err = s.freezeTx(ctx, tx, provider, repo, ref, actor, reason, decisionID, approvalID)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("freeze %s: %w", repo, err)
	}
	return frozen, nil
}

func (s *Store) freezeTx(ctx context.Context, tx *sql.Tx, provider, repo, ref, actor, reason string,
	decisionID, approvalID int64) (bool, error) {
	now := s.nowMillis()
	var dec sql.NullInt64
	if decisionID != 0 {
		dec = sql.NullInt64{Int64: decisionID, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO repo_state (provider, repo, frozen, frozen_ref, frozen_at, frozen_reason, frozen_decision, updated_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo) DO UPDATE SET
			frozen = 1, frozen_ref = excluded.frozen_ref, frozen_at = excluded.frozen_at,
			frozen_reason = excluded.frozen_reason, frozen_decision = excluded.frozen_decision,
			updated_at = excluded.updated_at
		WHERE repo_state.frozen = 0`,
		provider, repo, ref, now, reason, dec, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	snap, err := decisionContext(ctx, tx, decisionID)
	if err != nil {
		return false, err
	}
	return true, addAudit(ctx, tx, AuditEntry{Provider: provider, Repo: repo, Ref: ref, Kind: AuditFreeze, Actor: actor,
		Reason: reason, Detail: "frozen at " + orNone(ref), DecisionID: decisionID, ApprovalID: approvalID,
		Context: snap}, now)
}

// Unfreeze lifts a freeze and records who did it and why. It returns false
// if the repo wasn't frozen.
func (s *Store) Unfreeze(ctx context.Context, provider, repo, actor, reason string) (bool, error) {
	var unfrozen bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var ref string
		var decision sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(frozen_ref, ''), frozen_decision FROM repo_state
			WHERE provider = ? AND repo = ? AND frozen = 1`, provider, repo).Scan(&ref, &decision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := s.nowMillis()
		if _, err := tx.ExecContext(ctx, `UPDATE repo_state SET frozen = 0, updated_at = ?
			WHERE provider = ? AND repo = ?`, now, provider, repo); err != nil {
			return err
		}
		snap, err := decisionContext(ctx, tx, decision.Int64)
		if err != nil {
			return err
		}
		unfrozen = true
		return addAudit(ctx, tx, AuditEntry{Provider: provider, Repo: repo, Ref: ref, Kind: AuditUnfreeze, Actor: actor,
			Reason: reason, Detail: "unfrozen (was frozen at " + orNone(ref) + ")", DecisionID: decision.Int64,
			Context: snap}, now)
	})
	if err != nil {
		return false, fmt.Errorf("unfreeze %s: %w", repo, err)
	}
	return unfrozen, nil
}

func orNone(ref string) string {
	if ref == "" {
		return "no version"
	}
	return ref
}
