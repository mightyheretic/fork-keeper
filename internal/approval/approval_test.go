package approval

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/merge"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// fakeHost records PR changes. Methods the service doesn't use panic.
type fakeHost struct {
	provider.ForkHost
	pr       provider.PullRequest
	comments []string
	fail     error
}

func (h *fakeHost) MarkReadyForReview(context.Context, provider.Repo, int) error {
	if h.fail != nil {
		return h.fail
	}
	h.pr.Draft = false
	return nil
}

func (h *fakeHost) RemoveLabel(_ context.Context, _ provider.Repo, _ int, label string) error {
	h.pr.Labels = slices.DeleteFunc(h.pr.Labels, func(l string) bool { return l == label })
	return nil
}

func (h *fakeHost) Comment(_ context.Context, _ provider.Repo, _ int, body string) error {
	h.comments = append(h.comments, body)
	return nil
}

func (h *fakeHost) ClosePullRequest(context.Context, provider.Repo, int) error {
	h.pr.State = "closed"
	return nil
}

type env struct {
	svc  *Service
	st   *store.Store
	host *fakeHost
}

func setup(t *testing.T) env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	host := &fakeHost{pr: provider.PullRequest{Number: 7, State: "open", Draft: true,
		Labels: []string{merge.LabelSync, merge.LabelApproval}}}
	svc := New(st, map[string]provider.ForkHost{"github": host}, slog.New(slog.DiscardHandler))
	return env{svc: svc, st: st, host: host}
}

// version records a decision on ref that needs kind ("" for none) and,
// with synced, the sync PR that now carries it.
func (e env) version(t *testing.T, ref, prev, kind string, synced bool) store.Approval {
	t.Helper()
	ctx := context.Background()
	key := store.RefKey{Provider: "github", Repo: "up/app", Ref: ref}
	policy, pending := rules.MergeAuto, []string{}
	switch kind {
	case store.ApprovalNeeded:
		policy, pending = rules.MergeApproval, []string{"require_approval"}
	case store.ApprovalOverride:
		policy, pending = rules.MergeBlocked, []string{"block_auto_merge"}
	}
	id, err := e.st.SaveDecision(ctx, store.Decision{RefKey: key, PreviousRef: prev, Rule: "r", Actions: pending,
		MergePolicy: string(policy), Reason: "MIT -> GPL-3.0", Pending: pending})
	if err != nil {
		t.Fatal(err)
	}
	if synced {
		if err := e.st.SaveSync(ctx, store.Sync{RefKey: key, Fork: "me/app", Status: store.SyncPROpen,
			MergePolicy: string(rules.MergeApproval), PRNumber: 7, PRURL: "https://github.com/me/app/pull/7"}); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "" {
		return store.Approval{}
	}
	a, err := e.st.SaveApproval(ctx, key, id, kind)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApproveMakesPRMergeable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.version(t, "v2", "v1", store.ApprovalNeeded, true)
	res, err := e.svc.Approve(ctx, a.ID, "cli:me", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.host.pr.Draft || slices.Contains(e.host.pr.Labels, merge.LabelApproval) || len(e.host.comments) != 1 {
		t.Fatalf("PR = %+v, comments %v", e.host.pr, e.host.comments)
	}
	open, _, _ := e.st.OpenSyncFor(ctx, "github", "up/app")
	if open.MergePolicy != string(rules.MergeAuto) || !strings.Contains(res.Detail, "ready") {
		t.Fatalf("sync = %+v, detail %q", open, res.Detail)
	}
	if got, _, _ := e.st.Approval(ctx, a.ID); !got.Applied {
		t.Fatal("not marked applied")
	}
}

func TestApproveWaitsForEveryPendingVersion(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first := e.version(t, "v2", "v1", store.ApprovalNeeded, true)
	second := e.version(t, "v3", "v2", store.ApprovalNeeded, true)

	res, err := e.svc.Approve(ctx, second.ID, "cli:me", "")
	if err != nil || !e.host.pr.Draft || !strings.Contains(res.Detail, "still waiting on #"+strconv.FormatInt(first.ID, 10)) {
		t.Fatalf("approving one of two: %q %v, PR %+v", res.Detail, err, e.host.pr)
	}
	if _, err := e.svc.Approve(ctx, first.ID, "cli:me", ""); err != nil || e.host.pr.Draft {
		t.Fatalf("approving both: %v, PR %+v", err, e.host.pr)
	}
}

func TestOverrideNeedsReason(t *testing.T) {
	e := setup(t)
	a := e.version(t, "v2", "v1", store.ApprovalOverride, true)
	if _, err := e.svc.Approve(context.Background(), a.ID, "cli:me", ""); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("err = %v", err)
	}
	if got, _, _ := e.st.Approval(context.Background(), a.ID); got.Status != store.ApprovalPending {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestRejectFreezesAndClosesPR(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.version(t, "v2", "v1", store.ApprovalNeeded, true)
	if _, err := e.svc.Reject(ctx, a.ID, "cli:me", ""); err == nil {
		t.Fatal("rejected without a reason")
	}
	res, err := e.svc.Reject(ctx, a.ID, "cli:me", "we can't ship GPL")
	if err != nil || !res.Frozen {
		t.Fatalf("reject: %+v %v", res, err)
	}
	st, _ := e.st.RepoState(ctx, "github", "up/app")
	if !st.Frozen || st.FrozenRef != "v1" {
		t.Fatalf("state = %+v", st)
	}
	if e.host.pr.State != "closed" || len(e.host.comments) != 1 || !strings.Contains(e.host.comments[0], "rejected") {
		t.Fatalf("PR = %+v %v", e.host.pr, e.host.comments)
	}
	if _, ok, _ := e.st.OpenSyncFor(ctx, "github", "up/app"); ok {
		t.Fatal("sync still open")
	}
}

func TestLabelApprovesFromGitHub(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.version(t, "v2", "v1", store.ApprovalNeeded, false)
	e.version(t, "v3", "v2", "", true) // v3 carries v2's change on the same PR
	sync, _, _ := e.st.OpenSyncFor(ctx, "github", "up/app")

	pr := e.host.pr
	if changed, err := e.svc.CheckPR(ctx, sync, &pr); changed || err != nil {
		t.Fatalf("unlabeled PR: %v %v", changed, err)
	}
	pr.Labels = append(pr.Labels, notify.LabelApproved)
	if changed, err := e.svc.CheckPR(ctx, sync, &pr); !changed || err != nil {
		t.Fatalf("labeled PR: %v %v", changed, err)
	}
	got, _, _ := e.st.Approval(ctx, a.ID)
	if got.Status != store.ApprovalApproved || got.ResolvedBy != "github" || !got.Applied || e.host.pr.Draft {
		t.Fatalf("approval = %+v, PR %+v", got, e.host.pr)
	}
}

func TestMergedByHandApproves(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.version(t, "v2", "v1", store.ApprovalNeeded, true)
	sync, _, _ := e.st.OpenSyncFor(ctx, "github", "up/app")
	pr := provider.PullRequest{Number: 7, State: "closed", Merged: true}
	if changed, err := e.svc.CheckPR(ctx, sync, &pr); !changed || err != nil {
		t.Fatalf("merged: %v %v", changed, err)
	}
	got, _, _ := e.st.Approval(ctx, a.ID)
	if got.Status != store.ApprovalApproved || !got.Applied || !strings.Contains(got.Resolution, "merged") {
		t.Fatalf("approval = %+v", got)
	}
}

func TestFailedApplyIsRetried(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.version(t, "v2", "v1", store.ApprovalNeeded, true)
	e.host.fail = errFake
	if _, err := e.svc.Approve(ctx, a.ID, "cli:me", ""); err == nil {
		t.Fatal("no error")
	}
	if got, _, _ := e.st.Approval(ctx, a.ID); got.Status != store.ApprovalApproved || got.Applied {
		t.Fatalf("approval = %+v", got)
	}
	e.host.fail = nil
	if err := e.svc.ApplyPending(ctx); err != nil || e.host.pr.Draft {
		t.Fatalf("retry: %v, PR %+v", err, e.host.pr)
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errFake = fakeErr("GitHub is down")
