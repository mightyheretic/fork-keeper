// Package notify sends Fork Keeper's messages to ntfy, Gotify, email or a
// webhook. Messages go through an outbox in the store, so a failed send is
// retried and a retried event doesn't notify twice.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// Message kinds.
const (
	// KindLicense: a license rule fired (its actions include notify).
	KindLicense = "license"
	// KindApproval: a version needs approval.
	KindApproval = "approval"
	// KindReminder: an approval is still waiting.
	KindReminder = "reminder"
	// KindSync: a merge conflict or a CI failure on a sync PR.
	KindSync = "sync"
	// KindTest: forkkeeper notify test.
	KindTest = "test"
)

// Priority is how loudly a message should arrive.
type Priority string

const (
	PriorityLow     Priority = "low"
	PriorityDefault Priority = "default"
	PriorityHigh    Priority = "high"
	PriorityUrgent  Priority = "urgent"
)

// Link is an action button (ntfy, Gotify) or a link line (email).
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Message is what every channel sends.
type Message struct {
	Kind     string   `json:"kind"`
	Repo     string   `json:"repo,omitempty"`
	Ref      string   `json:"ref,omitempty"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Priority Priority `json:"priority"`
	Tags     []string `json:"tags,omitempty"`
	// Click is opened when the notification is tapped.
	Click string `json:"click,omitempty"`
	Links []Link `json:"links,omitempty"`
}

// Channel delivers messages to one service.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Channels builds the configured channels.
func Channels(n config.Notifications, creds credentials.Provider, hc *http.Client) []Channel {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	var out []Channel
	if n.Ntfy != nil {
		out = append(out, &Ntfy{URL: n.Ntfy.URL, Token: secretFunc(creds, n.Ntfy.Token), HTTP: hc})
	}
	if n.Gotify != nil {
		out = append(out, &Gotify{URL: n.Gotify.URL, Token: secretFunc(creds, n.Gotify.Token), HTTP: hc})
	}
	if n.Email != nil {
		e := n.Email
		out = append(out, &Email{Addr: e.SMTP, Username: e.Username, Password: secretFunc(creds, e.Password),
			From: e.From, To: e.To})
	}
	if n.Webhook != nil {
		out = append(out, &Webhook{URL: n.Webhook.URL, Token: secretFunc(creds, n.Webhook.Token), HTTP: hc})
	}
	return out
}

// SecretFunc reads a credential when a message is sent, so a rotated
// token is picked up without a restart. It returns an empty secret when
// no credential is configured.
type SecretFunc func(ctx context.Context) (credentials.Secret, error)

func secretFunc(creds credentials.Provider, name string) SecretFunc {
	if name == "" {
		return nil
	}
	return func(ctx context.Context) (credentials.Secret, error) { return creds.Get(ctx, name) }
}

func (f SecretFunc) get(ctx context.Context) (string, error) {
	if f == nil {
		return "", nil
	}
	s, err := f(ctx)
	if err != nil {
		return "", err
	}
	return s.Reveal(), nil
}

// Notice is a message to queue.
type Notice struct {
	// Key dedupes: a notice with the key of one already sent is dropped.
	Key        string
	Provider   string
	DecisionID int64
	Message    Message
}

// QueueStore is the subset of the store the queue uses.
type QueueStore interface {
	EnqueueNotification(ctx context.Context, n store.Notification) (bool, error)
}

// Queue puts notices in the outbox.
type Queue struct {
	store   QueueStore
	cfg     config.Notifications
	enabled bool
	// Wake, if set, is called after a notice is queued.
	Wake func()
}

// NewQueue returns a queue. With no channels configured, notices are
// dropped and rules' notify actions stay pending.
func NewQueue(st QueueStore, cfg config.Notifications) *Queue {
	return &Queue{store: st, cfg: cfg, enabled: cfg.Enabled()}
}

// Wants reports whether a notice of kind would be queued.
func (q *Queue) Wants(kind string) bool {
	if q == nil || !q.enabled {
		return false
	}
	switch kind {
	case KindApproval:
		return q.cfg.NotifyApprovals()
	case KindReminder:
		return q.cfg.Reminders() > 0
	case KindSync:
		return q.cfg.NotifySyncProblems()
	}
	return true
}

// Notify queues n. It reports false when n was dropped: notifications are
// off for its kind, or a notice with its key was already sent.
func (q *Queue) Notify(ctx context.Context, n Notice) (bool, error) {
	if !q.Wants(n.Message.Kind) {
		return false, nil
	}
	b, err := json.Marshal(n.Message)
	if err != nil {
		return false, err
	}
	queued, err := q.store.EnqueueNotification(ctx, store.Notification{Key: n.Key, Kind: n.Message.Kind,
		Provider: n.Provider, Repo: n.Message.Repo, Ref: n.Message.Ref, DecisionID: n.DecisionID, Message: b})
	if err != nil {
		return false, err
	}
	if queued && q.Wake != nil {
		q.Wake()
	}
	return queued, nil
}

// SenderStore is the subset of the store the sender uses.
type SenderStore interface {
	QueueStore
	DueNotifications(ctx context.Context, limit int) ([]store.Notification, error)
	RecordNotificationAttempt(ctx context.Context, id int64, sent []string, lastError string, done bool, next time.Time) error
	DueReminders(ctx context.Context, before time.Time) ([]store.Approval, error)
	MarkApprovalNotified(ctx context.Context, id int64, reminder bool) error
	SyncFor(ctx context.Context, key store.RefKey) (store.Sync, bool, error)
}

// Sender delivers the outbox and queues approval reminders.
type Sender struct {
	store    SenderStore
	queue    *Queue
	channels []Channel
	log      *slog.Logger
	// Interval between outbox checks (a queued notice wakes it sooner).
	Interval time.Duration
	// GiveUpAfter stops retrying a message this long after it was queued.
	GiveUpAfter time.Duration
	// RemindAfter is the reminder interval; 0 turns reminders off.
	RemindAfter time.Duration
	now         func() time.Time
	wake        chan struct{}
}

// NewSender returns a sender for the configured channels. It sets the
// queue's Wake.
func NewSender(st SenderStore, q *Queue, channels []Channel, cfg config.Notifications, log *slog.Logger) *Sender {
	s := &Sender{store: st, queue: q, channels: channels, log: log, Interval: time.Minute, GiveUpAfter: 24 * time.Hour,
		RemindAfter: cfg.Reminders(), now: time.Now, wake: make(chan struct{}, 1)}
	q.Wake = s.Wake
	return s
}

// Wake makes Run check the outbox now.
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run delivers until ctx is done.
func (s *Sender) Run(ctx context.Context) {
	if len(s.channels) == 0 {
		return
	}
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("sending notifications failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Once queues due reminders and sends every due message.
func (s *Sender) Once(ctx context.Context) error {
	var errs []error
	if s.RemindAfter > 0 {
		errs = append(errs, s.remind(ctx))
	}
	due, err := s.store.DueNotifications(ctx, 50)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, n := range due {
		errs = append(errs, s.deliver(ctx, n))
	}
	return errors.Join(errs...)
}

func (s *Sender) deliver(ctx context.Context, n store.Notification) error {
	var m Message
	if err := json.Unmarshal(n.Message, &m); err != nil {
		return s.store.RecordNotificationAttempt(ctx, n.ID, n.Sent, "unreadable message: "+err.Error(), true, s.now())
	}
	sent := slices.Clone(n.Sent)
	var failures []string
	for _, ch := range s.channels {
		if slices.Contains(sent, ch.Name()) {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := ch.Send(sendCtx, m)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ch.Name(), err))
			continue
		}
		sent = append(sent, ch.Name())
	}
	log := s.log.With("kind", n.Kind, "repo", n.Repo, "ref", n.Ref, "title", m.Title)
	if len(failures) == 0 {
		log.Info("notification sent", "channels", strings.Join(sent, ", "))
		return s.store.RecordNotificationAttempt(ctx, n.ID, sent, "", true, s.now())
	}
	lastErr := strings.Join(failures, "; ")
	if s.now().Sub(n.CreatedAt) >= s.GiveUpAfter {
		log.Error("giving up on notification", "attempts", n.Attempts+1, "err", lastErr)
		return s.store.RecordNotificationAttempt(ctx, n.ID, sent, lastErr, true, s.now())
	}
	delay := min(time.Minute<<min(n.Attempts, 6), time.Hour)
	log.Warn("notification failed; will retry", "attempts", n.Attempts+1, "retry_in", delay, "err", lastErr)
	return s.store.RecordNotificationAttempt(ctx, n.ID, sent, lastErr, false, s.now().Add(delay))
}

func (s *Sender) remind(ctx context.Context) error {
	due, err := s.store.DueReminders(ctx, s.now().Add(-s.RemindAfter))
	if err != nil {
		return err
	}
	for _, a := range due {
		sync, _, err := s.store.SyncFor(ctx, a.RefKey)
		if err != nil {
			return err
		}
		if _, err := s.queue.Notify(ctx, Notice{Key: fmt.Sprintf("reminder:%d:%d", a.ID, a.Reminders+1),
			Provider: a.Provider, Message: Reminder(a, sync.PRURL, s.now())}); err != nil {
			return err
		}
		if err := s.store.MarkApprovalNotified(ctx, a.ID, true); err != nil {
			return err
		}
	}
	return nil
}

// Send delivers m to every channel right away, without the outbox (for
// forkkeeper notify test). It returns one error per failed channel.
func Send(ctx context.Context, channels []Channel, m Message) map[string]error {
	out := map[string]error{}
	for _, ch := range channels {
		out[ch.Name()] = ch.Send(ctx, m)
	}
	return out
}

// DecisionContext is the decision snapshot stored with an approval.
type DecisionContext struct {
	Rule        string   `json:"rule"`
	Reason      string   `json:"reason"`
	Actions     []string `json:"actions"`
	MergePolicy string   `json:"merge_policy"`
	PreviousRef string   `json:"previous_ref"`
}

// Reminder is the message for an approval still waiting.
func Reminder(a store.Approval, prURL string, now time.Time) Message {
	var dc DecisionContext
	_ = json.Unmarshal(a.Context, &dc)
	waiting := now.Sub(a.CreatedAt).Round(time.Hour)
	verb := "approve"
	if a.Kind == store.ApprovalOverride {
		verb = "override the block on"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s has been waiting %s for you to %s it.\n", a.Repo, a.Ref, humanDuration(waiting), verb)
	if dc.Rule != "" {
		fmt.Fprintf(&b, "%s.\n", Capitalize(dc.Reason))
	}
	b.WriteString("\n" + ApprovalHelp(a.ID, a.Kind, prURL != ""))
	m := Message{Kind: KindReminder, Repo: a.Repo, Ref: a.Ref, Priority: PriorityDefault, Tags: []string{"hourglass"},
		Title: fmt.Sprintf("Reminder: %s %s needs approval (#%d)", a.Repo, a.Ref, a.ID), Body: b.String(), Click: prURL}
	if prURL != "" {
		m.Links = []Link{{Label: "Open PR", URL: prURL}}
	}
	return m
}

// ApprovalHelp tells the reader how to resolve approval id.
func ApprovalHelp(id int64, kind string, hasPR bool) string {
	flag := ""
	if kind == store.ApprovalOverride {
		flag = ` --reason "why"`
	}
	s := fmt.Sprintf("Approve: forkkeeper approve%s %d\nReject (freezes the fork): forkkeeper reject --reason \"why\" %d", flag, id, id)
	if hasPR {
		s += fmt.Sprintf("\nOr add the label %q to the PR.", LabelApproved)
	}
	return s
}

// LabelApproved approves the versions on a sync PR from GitHub.
const LabelApproved = "forkkeeper: approved"

// Capitalize upper-cases the first letter of s, to start a sentence.
func Capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func humanDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d hours", int(d.Hours()))
}
