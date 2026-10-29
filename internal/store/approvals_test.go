package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func saveTestDecision(t *testing.T, s *Store, ref, prev string, pending ...string) int64 {
	t.Helper()
	id, err := s.SaveDecision(context.Background(), Decision{
		RefKey: RefKey{Provider: "github", Repo: "up/app", Ref: ref}, PreviousRef: prev, Rule: "to-gpl",
		Scope: "global", Actions: append([]string{"notify"}, pending...), MergePolicy: "approval_required",
		Reason: "MIT -> GPL-3.0", Pending: append([]string{"notify"}, pending...),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApproveRecordsAuditAndSettlesDecision(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	key := RefKey{Provider: "github", Repo: "up/app", Ref: "v2"}
	id := saveTestDecision(t, s, "v2", "v1", "require_approval")
	a, err := s.SaveApproval(ctx, key, id, ApprovalNeeded)
	if err != nil || a.Status != ApprovalPending || a.ID == 0 {
		t.Fatalf("save: %+v %v", a, err)
	}
	var dc struct {
		PreviousRef string `json:"previous_ref"`
	}
	if err := json.Unmarshal(a.Context, &dc); err != nil || dc.PreviousRef != "v1" {
		t.Fatalf("context %s: %v", a.Context, err)
	}
	if p, _ := s.PendingApprovals(ctx, "github", "up/app"); len(p) != 1 {
		t.Fatalf("pending = %+v", p)
	}

	a, err = s.ResolveApproval(ctx, a.ID, Resolution{Approve: true, Actor: "cli:me", Reason: "GPL is fine"})
	if err != nil || a.Status != ApprovalApproved || a.Applied {
		t.Fatalf("approve: %+v %v", a, err)
	}
	if _, err := s.ResolveApproval(ctx, a.ID, Resolution{Actor: "cli:me", Reason: "changed my mind"}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second resolve: %v", err)
	}
	// Re-checking the version keeps the decision.
	if again, _ := s.SaveApproval(ctx, key, id, ApprovalNeeded); again.Status != ApprovalApproved {
		t.Fatalf("re-save reset the approval: %+v", again)
	}
	if u, _ := s.UnappliedApprovals(ctx); len(u) != 1 {
		t.Fatalf("unapplied = %+v", u)
	}
	s.MarkApprovalApplied(ctx, a.ID)
	if u, _ := s.UnappliedApprovals(ctx); len(u) != 0 {
		t.Fatalf("still unapplied: %+v", u)
	}

	ds, _ := s.Decisions(ctx, "up/app", 1)
	if !slices.Equal(ds[0].Pending, []string{"notify"}) {
		t.Fatalf("pending actions = %v", ds[0].Pending)
	}
	log, _ := s.AuditLog(ctx, "up/app", 10)
	if len(log) != 1 || log[0].Kind != AuditApprove || log[0].Actor != "cli:me" || log[0].Reason != "GPL is fine" ||
		log[0].ApprovalID != a.ID || log[0].DecisionID != id {
		t.Fatalf("audit = %+v", log)
	}
}

func TestRejectFreezes(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := saveTestDecision(t, s, "v2", "v1", "block_auto_merge")
	a, _ := s.SaveApproval(ctx, RefKey{Provider: "github", Repo: "up/app", Ref: "v2"}, id, ApprovalOverride)
	if _, err := s.ResolveApproval(ctx, a.ID, Resolution{Actor: "cli:me", Reason: "no", FreezeAt: "v1"}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.RepoState(ctx, "github", "up/app")
	if !st.Frozen || st.FrozenRef != "v1" || !strings.Contains(st.FrozenReason, "v2 rejected") {
		t.Fatalf("state = %+v", st)
	}
	log, _ := s.AuditLog(ctx, "", 10)
	if len(log) != 2 || log[0].Kind != AuditFreeze || log[1].Kind != AuditReject || log[0].ApprovalID != a.ID {
		t.Fatalf("audit = %+v", log)
	}
}

func TestFreezeAndUnfreezeAreAudited(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := saveTestDecision(t, s, "v3", "v2")
	if ok, _ := s.Freeze(ctx, "github", "up/app", "v2", "relicensed", id); !ok {
		t.Fatal("not frozen")
	}
	if ok, _ := s.Freeze(ctx, "github", "up/app", "v3", "again", id); ok {
		t.Fatal("frozen twice")
	}
	if ok, _ := s.Unfreeze(ctx, "github", "up/app", "cli:me", "upstream reverted"); !ok {
		t.Fatal("not unfrozen")
	}
	log, _ := s.AuditLog(ctx, "UP/app", 10)
	if len(log) != 2 || log[0].Kind != AuditUnfreeze || log[0].Reason != "upstream reverted" || log[0].DecisionID != id ||
		log[1].Kind != AuditFreeze || log[1].Actor != ActorForkKeeper || log[1].Detail != "frozen at v2" {
		t.Fatalf("audit = %+v", log)
	}
	var snap map[string]any
	if json.Unmarshal(log[1].Context, &snap); snap["rule"] != "to-gpl" {
		t.Fatalf("context = %s", log[1].Context)
	}
}

func TestReminders(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	id := saveTestDecision(t, s, "v2", "v1", "require_approval")
	a, _ := s.SaveApproval(ctx, RefKey{Provider: "github", Repo: "up/app", Ref: "v2"}, id, ApprovalNeeded)
	if due, _ := s.DueReminders(ctx, now.Add(-time.Hour)); len(due) != 0 {
		t.Fatalf("due too early: %+v", due)
	}
	now = now.Add(2 * time.Hour)
	if due, _ := s.DueReminders(ctx, now.Add(-time.Hour)); len(due) != 1 {
		t.Fatalf("not due: %+v", due)
	}
	s.MarkApprovalNotified(ctx, a.ID, true)
	if due, _ := s.DueReminders(ctx, now.Add(-time.Hour)); len(due) != 0 {
		t.Fatalf("due right after a reminder: %+v", due)
	}
	if got, _, _ := s.Approval(ctx, a.ID); got.Reminders != 1 {
		t.Fatalf("reminders = %d", got.Reminders)
	}
}

func TestNotificationOutbox(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id := saveTestDecision(t, s, "v2", "v1")
	n := Notification{Key: "decision:v2", Kind: "license", Repo: "up/app", Ref: "v2", DecisionID: id, Message: json.RawMessage(`{"title":"a"}`)}
	if ok, err := s.EnqueueNotification(ctx, n); !ok || err != nil {
		t.Fatalf("enqueue: %v %v", ok, err)
	}
	// Still waiting: the newer message replaces it.
	n.Message = json.RawMessage(`{"title":"b"}`)
	if ok, _ := s.EnqueueNotification(ctx, n); !ok {
		t.Fatal("replace refused")
	}
	due, _ := s.DueNotifications(ctx, 10)
	if len(due) != 1 || string(due[0].Message) != `{"title":"b"}` {
		t.Fatalf("due = %+v", due)
	}
	// One channel failed: retried later, only for that channel.
	if err := s.RecordNotificationAttempt(ctx, due[0].ID, []string{"ntfy"}, "email: down", false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueNotifications(ctx, 10); len(due) != 0 {
		t.Fatalf("due before retry time: %+v", due)
	}
	s.RecordNotificationAttempt(ctx, due[0].ID, []string{"ntfy", "email"}, "", true, time.Now())
	list, _ := s.Notifications(ctx, 10)
	if list[0].Status != NotifySent || list[0].Attempts != 2 || !slices.Equal(list[0].Sent, []string{"ntfy", "email"}) {
		t.Fatalf("after send: %+v", list[0])
	}
	ds, _ := s.Decisions(ctx, "up/app", 1)
	if len(ds[0].Pending) != 0 {
		t.Fatalf("notify still pending: %v", ds[0].Pending)
	}
	// Already sent: a retried event doesn't send it again.
	if ok, _ := s.EnqueueNotification(ctx, n); ok {
		t.Fatal("re-queued a sent notification")
	}
}

// TestMigrationBackfill re-runs migration 7 on a database with decisions
// and unfreeze records made before it existed.
func TestMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE approvals`, `DROP TABLE audit_log`, `DROP TABLE notifications`,
		`DELETE FROM schema_migrations WHERE version = 7`,
		`CREATE TABLE repo_events (id INTEGER PRIMARY KEY, provider TEXT NOT NULL, repo TEXT NOT NULL, kind TEXT NOT NULL,
			detail TEXT NOT NULL, actor TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`INSERT INTO repo_events (provider, repo, kind, detail, actor, created_at)
			VALUES ('github', 'up/app', 'unfreeze', 'unfrozen (was frozen at v1): ok', 'cli:me', 1)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	saveTestDecision(t, s, "v2", "v1", "require_approval")
	saveTestDecision(t, s, "v3", "v2", "block_auto_merge")
	saveTestDecision(t, s, "v4", "v3") // nothing to approve
	saveTestDecision(t, s, "main@abc", "v3", "require_approval")
	saveTestDecision(t, s, "v5", "v4", "require_approval")
	s.SaveSync(ctx, Sync{RefKey: RefKey{Provider: "github", Repo: "up/app", Ref: "v5"}, Fork: "me/app", Status: SyncMerged})
	s.Close()

	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, _ := s.Approvals(ctx, "", "", 10)
	var got []string
	for _, a := range list {
		got = append(got, a.Ref+":"+a.Kind)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"v2:approval", "v3:override"}) {
		t.Fatalf("backfilled approvals = %v", got)
	}
	log, _ := s.AuditLog(ctx, "", 10)
	if len(log) != 1 || log[0].Kind != AuditUnfreeze || log[0].Actor != "cli:me" {
		t.Fatalf("audit = %+v", log)
	}
}
