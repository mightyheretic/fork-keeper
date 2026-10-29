package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sync statuses.
const (
	SyncPROpen     = "pr_open"
	SyncMerged     = "merged"
	SyncClosed     = "closed"
	SyncSuperseded = "superseded"
	SyncUpToDate   = "up_to_date"
	SyncConflict   = "conflict"
	SyncSkipped    = "skipped"
)

// Patch is one of the fork's own commits, re-applied or carried by a merge.
type Patch struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// Sync is the merge engine's record for one upstream ref.
type Sync struct {
	ID int64
	RefKey
	Fork          string
	Status        string
	Strategy      string
	MergePolicy   string
	StableBranch  string
	StagingBranch string
	HeadSHA       string
	PRNumber      int
	PRURL         string
	Patches       []Patch
	Conflicts     []string
	Detail        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SaveSync records rec, replacing an earlier record for the same ref. When rec
// opens or updates a PR, older open records for that PR are superseded.
func (s *Store) SaveSync(ctx context.Context, rec Sync) error {
	patches, _ := json.Marshal(nonNil(rec.Patches))
	conflicts, _ := json.Marshal(nonNil(rec.Conflicts))
	var pr sql.NullInt64
	if rec.PRNumber > 0 {
		pr = sql.NullInt64{Int64: int64(rec.PRNumber), Valid: true}
	}
	now := s.nowMillis()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO syncs (provider, repo, ref, fork, status, strategy, merge_policy, stable_branch,
				staging_branch, head_sha, pr_number, pr_url, patches, conflicts, detail, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider, repo, ref) DO UPDATE SET
				fork = excluded.fork, status = excluded.status, strategy = excluded.strategy,
				merge_policy = excluded.merge_policy, stable_branch = excluded.stable_branch,
				staging_branch = excluded.staging_branch, head_sha = excluded.head_sha,
				pr_number = excluded.pr_number, pr_url = excluded.pr_url, patches = excluded.patches,
				conflicts = excluded.conflicts, detail = excluded.detail, updated_at = excluded.updated_at`,
			rec.Provider, rec.Repo, rec.Ref, rec.Fork, rec.Status, rec.Strategy, rec.MergePolicy, rec.StableBranch,
			rec.StagingBranch, rec.HeadSHA, pr, rec.PRURL, string(patches), string(conflicts), rec.Detail, now, now); err != nil {
			return fmt.Errorf("save sync: %w", err)
		}
		if rec.Status == SyncPROpen && pr.Valid {
			if _, err := tx.ExecContext(ctx, `
				UPDATE syncs SET status = ?, detail = 'replaced by ' || ?, updated_at = ?
				WHERE provider = ? AND fork = ? AND pr_number = ? AND status = ? AND ref != ?`,
				SyncSuperseded, rec.Ref, now, rec.Provider, rec.Fork, pr, SyncPROpen, rec.Ref); err != nil {
				return fmt.Errorf("supersede syncs: %w", err)
			}
		}
		return nil
	})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

const syncColumns = `id, provider, repo, ref, fork, status, strategy, merge_policy, stable_branch, staging_branch,
	head_sha, COALESCE(pr_number, 0), pr_url, patches, conflicts, detail, created_at, updated_at`

func scanSync(row interface{ Scan(...any) error }) (Sync, error) {
	var rec Sync
	var patches, conflicts string
	var created, updated int64
	if err := row.Scan(&rec.ID, &rec.Provider, &rec.Repo, &rec.Ref, &rec.Fork, &rec.Status, &rec.Strategy, &rec.MergePolicy,
		&rec.StableBranch, &rec.StagingBranch, &rec.HeadSHA, &rec.PRNumber, &rec.PRURL, &patches, &conflicts, &rec.Detail,
		&created, &updated); err != nil {
		return rec, err
	}
	json.Unmarshal([]byte(patches), &rec.Patches)
	json.Unmarshal([]byte(conflicts), &rec.Conflicts)
	rec.CreatedAt, rec.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return rec, nil
}

// SyncFor returns the record for one ref.
func (s *Store) SyncFor(ctx context.Context, key RefKey) (Sync, bool, error) {
	rec, err := scanSync(s.db.QueryRowContext(ctx, `SELECT `+syncColumns+` FROM syncs
		WHERE provider = ? AND repo = ? AND ref = ?`, key.Provider, key.Repo, key.Ref))
	if errors.Is(err, sql.ErrNoRows) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, fmt.Errorf("read sync: %w", err)
	}
	return rec, true, nil
}

// SyncedRefs returns the refs of repo that reached a fork (PR open or merged).
func (s *Store) SyncedRefs(ctx context.Context, provider, repo string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref FROM syncs WHERE provider = ? AND repo = ?
		AND status IN (?, ?, ?)`, provider, repo, SyncPROpen, SyncMerged, SyncSuperseded)
	if err != nil {
		return nil, fmt.Errorf("list synced refs: %w", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// OpenSyncs returns records whose PR is open.
func (s *Store) OpenSyncs(ctx context.Context) ([]Sync, error) {
	return s.listSyncs(ctx, `WHERE status = ? ORDER BY id`, SyncPROpen)
}

// Syncs returns recent records for repo ("" for all), newest first.
func (s *Store) Syncs(ctx context.Context, repo string, limit int) ([]Sync, error) {
	return s.listSyncs(ctx, `WHERE ? = '' OR repo = ? ORDER BY updated_at DESC, id DESC LIMIT ?`, repo, repo, limit)
}

func (s *Store) listSyncs(ctx context.Context, where string, args ...any) ([]Sync, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+syncColumns+` FROM syncs `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list syncs: %w", err)
	}
	defer rows.Close()
	var out []Sync
	for rows.Next() {
		rec, err := scanSync(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpdateSyncStatus changes a record's status and detail.
func (s *Store) UpdateSyncStatus(ctx context.Context, id int64, status, detail string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE syncs SET status = ?, detail = ?, updated_at = ? WHERE id = ?`,
		status, detail, s.nowMillis(), id); err != nil {
		return fmt.Errorf("update sync: %w", err)
	}
	return nil
}
