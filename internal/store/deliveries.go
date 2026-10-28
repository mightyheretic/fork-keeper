package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// Delivery statuses.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusFailed     = "failed"
)

// Job is a claimed delivery that a worker must finish with Complete or Fail.
type Job struct {
	ID       int64
	Event    provider.Event
	Attempts int
}

// EnqueueDelivery records a verified delivery for background processing.
// It returns false, without error, if the same provider and delivery ID was
// already recorded, so retries and replays are processed at most once.
func (s *Store) EnqueueDelivery(ctx context.Context, ev provider.Event) (bool, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return false, fmt.Errorf("encode event: %w", err)
	}
	now := s.nowMillis()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries
			(provider, delivery_id, kind, repo, event, next_attempt_at, received_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, delivery_id) DO NOTHING`,
		ev.Provider, ev.DeliveryID, string(ev.Kind), ev.Repo.FullName(), string(payload),
		now, ev.ReceivedAt.UnixMilli(), now)
	if err != nil {
		return false, fmt.Errorf("insert delivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ClaimNext marks the oldest due pending delivery as processing and returns
// it. ok is false when nothing is due.
func (s *Store) ClaimNext(ctx context.Context) (job Job, ok bool, err error) {
	now := s.nowMillis()
	var payload string
	err = s.db.QueryRowContext(ctx, `
		UPDATE webhook_deliveries
		SET status = 'processing', attempts = attempts + 1, updated_at = ?
		WHERE id = (
			SELECT id FROM webhook_deliveries
			WHERE status = 'pending' AND next_attempt_at <= ?
			ORDER BY next_attempt_at, id
			LIMIT 1
		)
		RETURNING id, event, attempts`, now, now).Scan(&job.ID, &payload, &job.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim delivery: %w", err)
	}
	if err := json.Unmarshal([]byte(payload), &job.Event); err != nil {
		return Job{}, false, fmt.Errorf("decode delivery %d: %w", job.ID, err)
	}
	return job, true, nil
}

// Complete marks a claimed delivery as done.
func (s *Store) Complete(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, StatusDone, nil, 0)
}

// Retry returns a claimed delivery to the queue, due again after delay.
func (s *Store) Retry(ctx context.Context, id int64, cause error, delay time.Duration) error {
	return s.setStatus(ctx, id, StatusPending, cause, delay)
}

// Fail marks a claimed delivery as permanently failed.
func (s *Store) Fail(ctx context.Context, id int64, cause error) error {
	return s.setStatus(ctx, id, StatusFailed, cause, 0)
}

func (s *Store) setStatus(ctx context.Context, id int64, status string, cause error, delay time.Duration) error {
	var lastErr sql.NullString
	if cause != nil {
		lastErr = sql.NullString{String: cause.Error(), Valid: true}
	}
	now := s.nowMillis()
	res, err := s.db.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET status = ?, last_error = ?, next_attempt_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing'`,
		status, lastErr, now+delay.Milliseconds(), now, id)
	if err != nil {
		return fmt.Errorf("update delivery %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("update delivery %d: not in processing state", id)
	}
	return nil
}

// RequeueStale returns deliveries left in processing (for example after a
// crash) to pending. Call it once at startup, before workers start.
func (s *Store) RequeueStale(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE webhook_deliveries SET status = 'pending', updated_at = ?
		WHERE status = 'processing'`, s.nowMillis())
	if err != nil {
		return 0, fmt.Errorf("requeue stale deliveries: %w", err)
	}
	return res.RowsAffected()
}

// DeliveryStatus returns the status and attempt count of a delivery.
func (s *Store) DeliveryStatus(ctx context.Context, providerName, deliveryID string) (status string, attempts int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT status, attempts FROM webhook_deliveries
		WHERE provider = ? AND delivery_id = ?`, providerName, deliveryID).Scan(&status, &attempts)
	return status, attempts, err
}
