package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// PollKey identifies what is being polled.
type PollKey struct {
	Provider string
	Repo     string // owner/name
	Kind     provider.EventKind
}

// PollState is the bookkeeping for one PollKey.
type PollState struct {
	ETag          string
	Baselined     bool
	LastSuccessAt time.Time
	LastError     string
}

// PollState returns the state for key, or the zero state if it was never polled.
func (s *Store) PollState(ctx context.Context, key PollKey) (PollState, error) {
	var st PollState
	var baselined int
	var lastSuccess sql.NullInt64
	var lastErr sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT etag, baselined, last_success_at, last_error FROM poll_state
		WHERE provider = ? AND repo = ? AND kind = ?`,
		key.Provider, key.Repo, string(key.Kind)).Scan(&st.ETag, &baselined, &lastSuccess, &lastErr)
	if errors.Is(err, sql.ErrNoRows) {
		return PollState{}, nil
	}
	if err != nil {
		return PollState{}, fmt.Errorf("read poll state: %w", err)
	}
	st.Baselined = baselined == 1
	if lastSuccess.Valid {
		st.LastSuccessAt = time.UnixMilli(lastSuccess.Int64)
	}
	st.LastError = lastErr.String
	return st, nil
}

// RecordPollUnchanged records a successful poll that found nothing new
// (for example a 304 Not Modified).
func (s *Store) RecordPollUnchanged(ctx context.Context, key PollKey) error {
	now := s.nowMillis()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO poll_state (provider, repo, kind, last_polled_at, last_success_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, kind) DO UPDATE SET
			last_polled_at = excluded.last_polled_at,
			last_success_at = excluded.last_success_at,
			last_error = NULL`,
		key.Provider, key.Repo, string(key.Kind), now, now)
	if err != nil {
		return fmt.Errorf("record poll: %w", err)
	}
	return nil
}

// RecordPollError records a failed poll.
func (s *Store) RecordPollError(ctx context.Context, key PollKey, cause error) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO poll_state (provider, repo, kind, last_polled_at, last_error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider, repo, kind) DO UPDATE SET
			last_polled_at = excluded.last_polled_at,
			last_error = excluded.last_error`,
		key.Provider, key.Repo, string(key.Kind), s.nowMillis(), cause.Error())
	if err != nil {
		return fmt.Errorf("record poll error: %w", err)
	}
	return nil
}

// PollOutcome summarizes RecordRefs.
type PollOutcome struct {
	// Baseline is true when this was the first successful poll: existing
	// refs were recorded as seen and nothing was queued.
	Baseline bool
	// New lists refs not seen before, in the order given.
	New []provider.Event
	// Queued lists the subset of New that was queued for processing.
	Queued []provider.Event
}

// RecordRefs stores the refs from a successful poll and, in the same
// transaction, queues an event for every ref not seen before that
// shouldQueue accepts. On the first poll for key (the baseline) nothing is
// queued, so adding a repo doesn't replay its whole history.
func (s *Store) RecordRefs(ctx context.Context, key PollKey, etag string, events []provider.Event,
	shouldQueue func(provider.Event) bool) (PollOutcome, error) {
	var out PollOutcome
	now := s.nowMillis()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var baselined int
		err := tx.QueryRowContext(ctx, `SELECT baselined FROM poll_state WHERE provider = ? AND repo = ? AND kind = ?`,
			key.Provider, key.Repo, string(key.Kind)).Scan(&baselined)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		out.Baseline = baselined == 0

		for _, ev := range events {
			var published sql.NullInt64
			if !ev.PublishedAt.IsZero() {
				published = sql.NullInt64{Int64: ev.PublishedAt.UnixMilli(), Valid: true}
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO upstream_refs
					(provider, repo, kind, name, title, prerelease, published_at, url, first_seen_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (provider, repo, kind, name) DO NOTHING`,
				key.Provider, key.Repo, string(key.Kind), ev.Tag, ev.ReleaseName, ev.Prerelease,
				published, ev.URL, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			out.New = append(out.New, ev)
			if out.Baseline || !shouldQueue(ev) {
				continue
			}
			inserted, err := enqueue(ctx, tx, ev, now)
			if err != nil {
				return err
			}
			if inserted {
				out.Queued = append(out.Queued, ev)
			}
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO poll_state (provider, repo, kind, etag, baselined, last_polled_at, last_success_at)
			VALUES (?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT (provider, repo, kind) DO UPDATE SET
				etag = excluded.etag,
				baselined = 1,
				last_polled_at = excluded.last_polled_at,
				last_success_at = excluded.last_success_at,
				last_error = NULL`,
			key.Provider, key.Repo, string(key.Kind), etag, now, now)
		return err
	})
	if err != nil {
		return PollOutcome{}, fmt.Errorf("record poll of %s: %w", key.Repo, err)
	}
	return out, nil
}
