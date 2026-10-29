package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// Archived ref kinds.
const (
	ArchiveRelease  = "release"
	ArchiveLastGood = "last-good"
	ArchiveLastOpen = "last-open"
)

// ArchivedRef is one ref in a repo's local source backup.
type ArchivedRef struct {
	Provider  string
	Repo      string
	Name      string // tag name, or forkkeeper/last-good, forkkeeper/last-open
	Kind      string
	CommitSHA string
	Reason    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SaveArchivedRef records a backed-up ref, replacing an earlier record of
// the same name (last-good and last-open move; tags never do).
func (s *Store) SaveArchivedRef(ctx context.Context, r ArchivedRef) error {
	now := s.nowMillis()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO archived_refs (provider, repo, name, kind, commit_sha, reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, name) DO UPDATE SET
			kind = excluded.kind, commit_sha = excluded.commit_sha, reason = excluded.reason,
			updated_at = excluded.updated_at`,
		r.Provider, r.Repo, r.Name, r.Kind, r.CommitSHA, r.Reason, now, now)
	if err != nil {
		return fmt.Errorf("save archived ref: %w", err)
	}
	return nil
}

// ArchivedRefs lists repo's backed-up refs ("" for every repo), by repo and
// then version.
func (s *Store) ArchivedRefs(ctx context.Context, providerName, repo string) ([]ArchivedRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, repo, name, kind, commit_sha, reason, created_at, updated_at FROM archived_refs
		WHERE (? = '' OR provider = ?) AND (? = '' OR repo = ?)`, providerName, providerName, repo, repo)
	if err != nil {
		return nil, fmt.Errorf("list archived refs: %w", err)
	}
	defer rows.Close()
	var out []ArchivedRef
	for rows.Next() {
		var r ArchivedRef
		var created, updated int64
		if err := rows.Scan(&r.Provider, &r.Repo, &r.Name, &r.Kind, &r.CommitSHA, &r.Reason, &created, &updated); err != nil {
			return nil, err
		}
		r.CreatedAt, r.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b ArchivedRef) int {
		if a.Repo != b.Repo {
			if a.Repo < b.Repo {
				return -1
			}
			return 1
		}
		return provider.CompareVersions(a.Name, b.Name)
	})
	return out, rows.Err()
}

// ArchivePush is the last push of a repo's backups to one destination.
type ArchivePush struct {
	Provider    string
	Repo        string
	Destination string
	OK          bool
	Refs        int
	Error       string
	PushedAt    time.Time
}

// SaveArchivePush records a push attempt.
func (s *Store) SaveArchivePush(ctx context.Context, p ArchivePush) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO archive_pushes (provider, repo, destination, ok, refs, error, pushed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, destination) DO UPDATE SET
			ok = excluded.ok, refs = excluded.refs, error = excluded.error, pushed_at = excluded.pushed_at`,
		p.Provider, p.Repo, p.Destination, p.OK, p.Refs, p.Error, s.nowMillis())
	if err != nil {
		return fmt.Errorf("save archive push: %w", err)
	}
	return nil
}

// ArchivePushes lists the push records for repo ("" for all).
func (s *Store) ArchivePushes(ctx context.Context, repo string) ([]ArchivePush, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, repo, destination, ok, refs, error, pushed_at FROM archive_pushes
		WHERE ? = '' OR repo = ? ORDER BY repo, destination`, repo, repo)
	if err != nil {
		return nil, fmt.Errorf("list archive pushes: %w", err)
	}
	defer rows.Close()
	var out []ArchivePush
	for rows.Next() {
		var p ArchivePush
		var at int64
		if err := rows.Scan(&p.Provider, &p.Repo, &p.Destination, &p.OK, &p.Refs, &p.Error, &at); err != nil {
			return nil, err
		}
		p.PushedAt = time.UnixMilli(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SWHRequest is a Software Heritage save request.
type SWHRequest struct {
	Provider    string
	Repo        string
	OriginURL   string
	RequestID   int64
	Status      string
	TaskStatus  string
	Reason      string
	RequestedAt time.Time
}

// SaveSWHRequest records a save request.
func (s *Store) SaveSWHRequest(ctx context.Context, r SWHRequest) error {
	var id sql.NullInt64
	if r.RequestID != 0 {
		id = sql.NullInt64{Int64: r.RequestID, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO swh_requests (provider, repo, origin_url, request_id, status, task_status, reason, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Provider, r.Repo, r.OriginURL, id, r.Status, r.TaskStatus, r.Reason, s.nowMillis())
	if err != nil {
		return fmt.Errorf("save Software Heritage request: %w", err)
	}
	return nil
}

// LastSWHRequest returns the newest save request for repo.
func (s *Store) LastSWHRequest(ctx context.Context, providerName, repo string) (SWHRequest, bool, error) {
	r := SWHRequest{Provider: providerName, Repo: repo}
	var id sql.NullInt64
	var at int64
	err := s.db.QueryRowContext(ctx, `
		SELECT origin_url, request_id, status, task_status, reason, requested_at FROM swh_requests
		WHERE provider = ? AND repo = ? ORDER BY requested_at DESC, id DESC LIMIT 1`, providerName, repo).
		Scan(&r.OriginURL, &id, &r.Status, &r.TaskStatus, &r.Reason, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("read Software Heritage request: %w", err)
	}
	r.RequestID, r.RequestedAt = id.Int64, time.UnixMilli(at)
	return r, true, nil
}

// MainWatch is the early-warning state for one upstream's default branch.
type MainWatch struct {
	Provider string
	Repo     string
	ETag     string
	HeadSHA  string
	// Fingerprint identifies the top-level license files and manifests at
	// HeadSHA, so an unchanged set isn't downloaded again.
	Fingerprint string
	// Report is the top-level license report at HeadSHA (JSON).
	Report []byte
	// BaseRef names what Report describes: a commit, or the release used
	// as the first baseline.
	BaseRef   string
	CheckedAt time.Time
	LastError string
}

// MainWatch returns the state for repo, or the zero state.
func (s *Store) MainWatch(ctx context.Context, providerName, repo string) (MainWatch, error) {
	w := MainWatch{Provider: providerName, Repo: repo}
	var report string
	var at sql.NullInt64
	var lastErr sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT etag, head_sha, fingerprint, report, base_ref, checked_at, last_error FROM main_watch
		WHERE provider = ? AND repo = ?`, providerName, repo).
		Scan(&w.ETag, &w.HeadSHA, &w.Fingerprint, &report, &w.BaseRef, &at, &lastErr)
	if errors.Is(err, sql.ErrNoRows) {
		return w, nil
	}
	if err != nil {
		return w, fmt.Errorf("read main watch: %w", err)
	}
	if report != "" {
		w.Report = []byte(report)
	}
	if at.Valid {
		w.CheckedAt = time.UnixMilli(at.Int64)
	}
	w.LastError = lastErr.String
	return w, nil
}

// SaveMainWatch stores a successful check.
func (s *Store) SaveMainWatch(ctx context.Context, w MainWatch) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO main_watch (provider, repo, etag, head_sha, fingerprint, report, base_ref, checked_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (provider, repo) DO UPDATE SET
			etag = excluded.etag, head_sha = excluded.head_sha, fingerprint = excluded.fingerprint,
			report = excluded.report, base_ref = excluded.base_ref, checked_at = excluded.checked_at,
			last_error = NULL`,
		w.Provider, w.Repo, w.ETag, w.HeadSHA, w.Fingerprint, string(w.Report), w.BaseRef, s.nowMillis())
	if err != nil {
		return fmt.Errorf("save main watch: %w", err)
	}
	return nil
}

// RecordMainWatchError records a failed check, keeping the last good state.
func (s *Store) RecordMainWatchError(ctx context.Context, providerName, repo string, cause error) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO main_watch (provider, repo, checked_at, last_error) VALUES (?, ?, ?, ?)
		ON CONFLICT (provider, repo) DO UPDATE SET checked_at = excluded.checked_at, last_error = excluded.last_error`,
		providerName, repo, s.nowMillis(), cause.Error())
	if err != nil {
		return fmt.Errorf("record main watch error: %w", err)
	}
	return nil
}

// CompletePending removes action from a decision's pending list.
func (s *Store) CompletePending(ctx context.Context, decisionID int64, action string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT pending FROM license_decisions WHERE id = ?`, decisionID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read pending actions: %w", err)
		}
		var pending []string
		_ = json.Unmarshal([]byte(raw), &pending)
		pending = slices.DeleteFunc(pending, func(a string) bool { return a == action })
		if pending == nil {
			pending = []string{}
		}
		b, _ := json.Marshal(pending)
		_, err = tx.ExecContext(ctx, `UPDATE license_decisions SET pending = ? WHERE id = ?`, string(b), decisionID)
		return err
	})
}

// LatestRef returns the newest non-prerelease ref of kind seen for repo.
func (s *Store) LatestRef(ctx context.Context, providerName, repo string, kind provider.EventKind) (string, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, COALESCE(published_at, 0) FROM upstream_refs
		WHERE provider = ? AND repo = ? AND kind = ? AND prerelease = 0`, providerName, repo, string(kind))
	if err != nil {
		return "", false, fmt.Errorf("list refs: %w", err)
	}
	defer rows.Close()
	var cands []refCand
	for rows.Next() {
		var c refCand
		if err := rows.Scan(&c.name, &c.published); err != nil {
			return "", false, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	var best *refCand
	for i := range cands {
		if best == nil || cands[i].newerThan(*best) {
			best = &cands[i]
		}
	}
	if best == nil {
		return "", false, nil
	}
	return best.name, true, nil
}
