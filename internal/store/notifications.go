package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Notification statuses.
const (
	NotifyPending = "pending"
	NotifySent    = "sent"
	NotifyFailed  = "failed"
)

// Notification is one message in the outbox.
type Notification struct {
	ID       int64
	Key      string
	Kind     string
	Provider string
	Repo     string
	Ref      string
	// DecisionID, if set, has its notify action completed once sent.
	DecisionID int64
	Message    json.RawMessage
	Status     string
	// Sent lists the channels that already have it.
	Sent        []string
	Attempts    int
	LastError   string
	NextAttempt time.Time
	CreatedAt   time.Time
	SentAt      time.Time
}

// EnqueueNotification adds n to the outbox. A message with the same key
// that is still waiting is replaced (it may carry newer details, like a PR
// link); one already sent is left alone, and EnqueueNotification returns
// false.
func (s *Store) EnqueueNotification(ctx context.Context, n Notification) (bool, error) {
	now := s.nowMillis()
	dec := sql.NullInt64{Int64: n.DecisionID, Valid: n.DecisionID != 0}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO notifications (key, kind, provider, repo, ref, decision_id, message, next_attempt, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET message = excluded.message, decision_id = excluded.decision_id
		WHERE notifications.status = 'pending'`,
		n.Key, n.Kind, n.Provider, n.Repo, n.Ref, dec, string(n.Message), now, now)
	if err != nil {
		return false, fmt.Errorf("queue notification: %w", err)
	}
	rows, _ := res.RowsAffected()
	return rows == 1, nil
}

const notificationColumns = `id, key, kind, provider, repo, ref, COALESCE(decision_id, 0), message, status, sent,
	attempts, last_error, next_attempt, created_at, COALESCE(sent_at, 0)`

func scanNotification(row interface{ Scan(...any) error }) (Notification, error) {
	var n Notification
	var msg, sent string
	var next, created, sentAt int64
	if err := row.Scan(&n.ID, &n.Key, &n.Kind, &n.Provider, &n.Repo, &n.Ref, &n.DecisionID, &msg, &n.Status, &sent,
		&n.Attempts, &n.LastError, &next, &created, &sentAt); err != nil {
		return n, err
	}
	n.Message = json.RawMessage(msg)
	_ = json.Unmarshal([]byte(sent), &n.Sent)
	n.NextAttempt, n.CreatedAt = time.UnixMilli(next), time.UnixMilli(created)
	if sentAt != 0 {
		n.SentAt = time.UnixMilli(sentAt)
	}
	return n, nil
}

// DueNotifications returns waiting messages whose next attempt is due.
func (s *Store) DueNotifications(ctx context.Context, limit int) ([]Notification, error) {
	return s.listNotifications(ctx, `WHERE status = ? AND next_attempt <= ? ORDER BY next_attempt, id LIMIT ?`,
		NotifyPending, s.nowMillis(), limit)
}

// Notifications returns recent messages, newest first.
func (s *Store) Notifications(ctx context.Context, limit int) ([]Notification, error) {
	return s.listNotifications(ctx, `ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
}

func (s *Store) listNotifications(ctx context.Context, where string, args ...any) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+notificationColumns+` FROM notifications `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// RecordNotificationAttempt saves the outcome of one send. With done set,
// the message is finished: sent (or failed, if lastError is set) and, when
// sent, its decision's notify action is completed. Otherwise it is retried
// at next.
func (s *Store) RecordNotificationAttempt(ctx context.Context, id int64, sent []string, lastError string, done bool,
	next time.Time) error {
	b, _ := json.Marshal(nonNil(sent))
	return s.inTx(ctx, func(tx *sql.Tx) error {
		status := NotifyPending
		var sentAt sql.NullInt64
		switch {
		case done && lastError == "":
			status, sentAt = NotifySent, sql.NullInt64{Int64: s.nowMillis(), Valid: true}
		case done:
			status = NotifyFailed
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notifications SET status = ?, sent = ?, attempts = attempts + 1,
			last_error = ?, next_attempt = ?, sent_at = ? WHERE id = ?`,
			status, string(b), lastError, next.UnixMilli(), sentAt, id); err != nil {
			return fmt.Errorf("record notification: %w", err)
		}
		if status != NotifySent {
			return nil
		}
		var decision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(decision_id, 0) FROM notifications WHERE id = ?`,
			id).Scan(&decision); err != nil || decision == 0 {
			return err
		}
		return completePendingTx(ctx, tx, decision, "notify")
	})
}
