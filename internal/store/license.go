package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// RefKey identifies one ref of an upstream repo.
type RefKey struct {
	Provider string
	Repo     string // owner/name
	Ref      string
}

// LicenseReport returns the cached report JSON for key, if any.
func (s *Store) LicenseReport(ctx context.Context, key RefKey) ([]byte, bool, error) {
	var report string
	err := s.db.QueryRowContext(ctx, `
		SELECT report FROM license_reports WHERE provider = ? AND repo = ? AND ref = ?`,
		key.Provider, key.Repo, key.Ref).Scan(&report)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read license report: %w", err)
	}
	return []byte(report), true, nil
}

// SaveLicenseReport caches the report JSON for key.
func (s *Store) SaveLicenseReport(ctx context.Context, key RefKey, report []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO license_reports (provider, repo, ref, report, scanned_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, ref) DO UPDATE SET report = excluded.report, scanned_at = excluded.scanned_at`,
		key.Provider, key.Repo, key.Ref, string(report), s.nowMillis())
	if err != nil {
		return fmt.Errorf("save license report: %w", err)
	}
	return nil
}

// LicenseCheck is the recorded result of a license gate run.
type LicenseCheck struct {
	RefKey
	PreviousRef string // "" for the first version seen
	Changed     bool
	Summary     string
	Diff        []byte // license.Diff JSON; nil without a previous ref
	CheckedAt   time.Time
}

// SaveLicenseCheck records (or replaces) the check result for a ref.
func (s *Store) SaveLicenseCheck(ctx context.Context, c LicenseCheck) error {
	var prev, diff sql.NullString
	if c.PreviousRef != "" {
		prev = sql.NullString{String: c.PreviousRef, Valid: true}
	}
	if c.Diff != nil {
		diff = sql.NullString{String: string(c.Diff), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO license_checks (provider, repo, ref, previous_ref, changed, summary, diff, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, ref) DO UPDATE SET
			previous_ref = excluded.previous_ref, changed = excluded.changed,
			summary = excluded.summary, diff = excluded.diff, checked_at = excluded.checked_at`,
		c.Provider, c.Repo, c.Ref, prev, c.Changed, c.Summary, diff, s.nowMillis())
	if err != nil {
		return fmt.Errorf("save license check: %w", err)
	}
	return nil
}

// LicenseChecks returns recent check results for repo ("" for all repos),
// newest first.
func (s *Store) LicenseChecks(ctx context.Context, repo string, limit int) ([]LicenseCheck, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, repo, ref, COALESCE(previous_ref, ''), changed, summary, diff, checked_at
		FROM license_checks WHERE ? = '' OR repo = ?
		ORDER BY checked_at DESC LIMIT ?`, repo, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("list license checks: %w", err)
	}
	defer rows.Close()
	var out []LicenseCheck
	for rows.Next() {
		var c LicenseCheck
		var diff sql.NullString
		var at int64
		if err := rows.Scan(&c.Provider, &c.Repo, &c.Ref, &c.PreviousRef, &c.Changed, &c.Summary, &diff, &at); err != nil {
			return nil, err
		}
		if diff.Valid {
			c.Diff = []byte(diff.String)
		}
		c.CheckedAt = time.UnixMilli(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PreviousRef returns the newest stable ref of the given kind that comes
// before ref in version order, so v1.17.1 is compared with v1.17.0 even
// if a v1.16.x backport was published in between. Publish dates are only
// used for names that aren't version numbers. publishedAt is ref's own
// publish time, used if ref isn't recorded yet.
func (s *Store) PreviousRef(ctx context.Context, key RefKey, kind provider.EventKind, publishedAt time.Time) (string, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, COALESCE(published_at, 0) FROM upstream_refs
		WHERE provider = ? AND repo = ? AND kind = ? AND prerelease = 0`,
		key.Provider, key.Repo, string(kind))
	if err != nil {
		return "", false, fmt.Errorf("list refs: %w", err)
	}
	defer rows.Close()

	type cand struct {
		name      string
		published int64
	}
	var cands []cand
	cur := publishedAt.UnixMilli()
	if publishedAt.IsZero() {
		cur = 0
	}
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.name, &c.published); err != nil {
			return "", false, err
		}
		if c.name == key.Ref {
			if cur == 0 {
				cur = c.published
			}
			continue
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}

	versioned := func(name string) bool { return strings.ContainsAny(name, "0123456789") }
	before := func(c cand) bool {
		if (!versioned(c.name) || !versioned(key.Ref)) && cur > 0 && c.published > 0 {
			return c.published < cur
		}
		return provider.CompareVersions(c.name, key.Ref) < 0
	}
	newer := func(a, b cand) bool { // is a newer than b?
		if c := provider.CompareVersions(a.name, b.name); c != 0 && versioned(a.name) && versioned(b.name) {
			return c > 0
		}
		return a.published > b.published
	}
	var best *cand
	for i := range cands {
		c := &cands[i]
		if before(*c) && (best == nil || newer(*c, *best)) {
			best = c
		}
	}
	if best == nil {
		return "", false, nil
	}
	return best.name, true, nil
}
