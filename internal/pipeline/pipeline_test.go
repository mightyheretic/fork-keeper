package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/archive"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/gate"
	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/merge"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// fakeGate returns canned license results per tag.
type fakeGate map[string]gate.Result

func (f fakeGate) Check(_ context.Context, ev provider.Event) (gate.Result, error) {
	r, ok := f[ev.Tag]
	if !ok {
		return gate.Result{}, provider.ErrRefNotFound
	}
	return r, nil
}

func result(prev string, old, cur []string, ref string) gate.Result {
	rep := &license.Report{Licenses: cur, Declared: cur[0]}
	if prev == "" {
		return gate.Result{Ref: ref, Report: rep}
	}
	oldRep := &license.Report{Licenses: old, Declared: old[0]}
	return gate.Result{Ref: ref, Previous: prev, Report: rep, Diff: license.Compare(prev, oldRep, ref, rep)}
}

func setup(t *testing.T, g fakeGate) (*Pipeline, *store.Store) {
	return setupWith(t, g, "", nil)
}

func setupWith(t *testing.T, g fakeGate, fork string, syncer Syncer) (*Pipeline, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
credentials: {provider: env}
repos:
  - upstream: up/app
    fork: "` + fork + `"
license:
  allowlist: [MIT]
  rules:
    - name: relicense
      from: [MIT]
      to: [BUSL-1.1, SSPL-1.0]
      actions: [freeze, archive, notify]
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(cfg, g, rules.New(cfg), s, syncer, slog.New(slog.DiscardHandler)), s
}

type fakeSyncer struct{ reqs []merge.Request }

func (f *fakeSyncer) Sync(_ context.Context, req merge.Request) (merge.Result, error) {
	f.reqs = append(f.reqs, req)
	return merge.Result{Sync: store.Sync{Status: store.SyncPROpen}}, nil
}

func TestSyncsForkUntilFrozen(t *testing.T) {
	ctx := context.Background()
	syncer := &fakeSyncer{}
	p, _ := setupWith(t, fakeGate{
		"v2": result("v1", []string{"MIT"}, []string{"MIT"}, "v2"),
		"v3": result("v2", []string{"MIT"}, []string{"BUSL-1.1"}, "v3"),
		"v4": result("v3", []string{"BUSL-1.1"}, []string{"BUSL-1.1"}, "v4"),
	}, "me/app", syncer)

	out, err := p.Run(ctx, event("v2"))
	if err != nil || out.Sync == nil || len(syncer.reqs) != 1 {
		t.Fatalf("v2: out=%+v err=%v", out, err)
	}
	req := syncer.reqs[0]
	if req.Fork.FullName() != "me/app" || req.Ref != "v2" || req.PreviousRef != "v1" ||
		req.Decision.MergePolicy != rules.MergeAuto || req.Repo.StagingBranch != config.DefaultStagingBranch {
		t.Fatalf("request = %+v", req)
	}
	// The relicense freezes the repo; neither it nor later versions are synced.
	for _, tag := range []string{"v3", "v4"} {
		if out, err := p.Run(ctx, event(tag)); err != nil || out.Sync != nil {
			t.Fatalf("%s: out=%+v err=%v", tag, out, err)
		}
	}
	if len(syncer.reqs) != 1 {
		t.Fatalf("synced %d times, want 1", len(syncer.reqs))
	}
}

func event(tag string) provider.Event {
	return provider.Event{Provider: "github", Kind: provider.KindRelease, Repo: provider.Repo{Owner: "up", Name: "app"}, Tag: tag}
}

func TestFreezeAtLastGoodVersion(t *testing.T) {
	ctx := context.Background()
	p, s := setup(t, fakeGate{
		"v1": result("", nil, []string{"MIT"}, "v1"),
		"v2": result("v1", []string{"MIT"}, []string{"MIT"}, "v2"),
		"v3": result("v2", []string{"MIT"}, []string{"BUSL-1.1"}, "v3"),
		"v4": result("v3", []string{"MIT"}, []string{"SSPL-1.0"}, "v4"),
	})

	for _, tag := range []string{"v1", "v2"} {
		out, err := p.Run(ctx, event(tag))
		if err != nil || out.Decision.Rule != "" || out.State.Frozen {
			t.Fatalf("%s: out=%+v err=%v", tag, out, err)
		}
	}

	out, err := p.Run(ctx, event("v3"))
	if err != nil || !out.NewlyFrozen || out.State.FrozenRef != "v2" || out.Decision.MergePolicy != rules.MergeFrozen {
		t.Fatalf("v3: out=%+v err=%v", out, err)
	}

	// A later change keeps the original freeze point.
	out, err = p.Run(ctx, event("v4"))
	if err != nil || out.NewlyFrozen || !out.State.Frozen || out.State.FrozenRef != "v2" {
		t.Fatalf("v4: out=%+v err=%v", out, err)
	}

	decisions, _ := s.Decisions(ctx, "up/app", 10)
	if len(decisions) != 4 {
		t.Fatalf("recorded %d decisions", len(decisions))
	}
	var v3 store.Decision
	for _, d := range decisions {
		if d.Ref == "v3" {
			v3 = d
		}
	}
	// freeze happened here; archive and notify wait for later stages.
	if !slices.Equal(v3.Pending, []string{"archive", "notify"}) || v3.PreviousRef != "v2" || v3.Rule != "relicense" {
		t.Fatalf("v3 decision = %+v", v3)
	}

	// Unfreezing records who and why; a repeat is a no-op.
	if ok, err := s.Unfreeze(ctx, "github", "up/app", "test", "upstream reverted"); !ok || err != nil {
		t.Fatalf("unfreeze: %v %v", ok, err)
	}
	if ok, _ := s.Unfreeze(ctx, "github", "up/app", "test", "again"); ok {
		t.Fatal("second unfreeze should report not frozen")
	}
	if st, _ := s.RepoState(ctx, "github", "up/app"); st.Frozen {
		t.Fatal("still frozen")
	}
}

func TestFirstVersionOutsideAllowlist(t *testing.T) {
	p, _ := setup(t, fakeGate{"v1": result("", nil, []string{"BUSL-1.1"}, "v1")})
	out, err := p.Run(context.Background(), event("v1"))
	if err != nil || out.Decision.Rule != "default" || out.Decision.MergePolicy != rules.MergeApproval || out.State.Frozen {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestSkipsAndPermanentErrors(t *testing.T) {
	p, _ := setup(t, fakeGate{})
	other := event("v1")
	other.Repo = provider.Repo{Owner: "someone", Name: "else"}
	if out, err := p.Run(context.Background(), other); err != nil || out.Skipped == "" {
		t.Fatalf("unconfigured repo: out=%+v err=%v", out, err)
	}
	ping := event("v1")
	ping.Kind = provider.KindPing
	if out, _ := p.Run(context.Background(), ping); out.Skipped == "" {
		t.Fatal("ping should be skipped")
	}

	err := p.Process(context.Background(), event("missing"))
	if err == nil {
		t.Fatal("missing ref should fail")
	}
	// A ref that doesn't exist won't appear on retry; the worker should give up.
	var perm interface{ Unwrap() error }
	if !errors.As(err, &perm) || !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("err = %v, want a permanent ErrRefNotFound", err)
	}
}

type fakeArchiver struct{ reqs []archive.Request }

func (f *fakeArchiver) Backup(_ context.Context, req archive.Request) (archive.Result, error) {
	f.reqs = append(f.reqs, req)
	return archive.Result{Added: append(req.Tags, req.LastGood)}, nil
}

func TestBacksUpReleasesAndLastGoodVersion(t *testing.T) {
	ctx := context.Background()
	p, s := setupWith(t, fakeGate{
		"v1.0.0": result("", nil, []string{"MIT"}, "v1.0.0"),
		"v1.0.1": result("v1.0.0", []string{"MIT"}, []string{"MIT"}, "v1.0.1"),
		"v1.1.0": result("v1.0.1", []string{"MIT"}, []string{"MIT"}, "v1.1.0"),
		"v1.1.1": result("v1.1.0", []string{"MIT"}, []string{"MIT"}, "v1.1.1"),
		"v2.0.0": result("v1.1.1", []string{"MIT"}, []string{"BUSL-1.1"}, "v2.0.0"),
		"v2.1.0": result("v2.0.0", []string{"BUSL-1.1"}, []string{"BUSL-1.1"}, "v2.1.0"),
	}, "me/app", &fakeSyncer{})
	arch := &fakeArchiver{}
	p.Archiver = arch

	for _, tag := range []string{"v1.0.0", "v1.0.1", "v1.1.0", "v1.1.1", "v2.0.0", "v2.1.0"} {
		if _, err := p.Run(ctx, event(tag)); err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
	}
	// The default policy (minor) backs up X.Y.0 releases; the relicense
	// backs up the version before it, a patch release, as the last good
	// one; nothing is backed up once frozen.
	var got []string
	for _, r := range arch.reqs {
		if r.Fork != "me/app" || !r.Settings.PushToFork {
			t.Fatalf("request: %+v", r)
		}
		for _, tag := range r.Tags {
			got = append(got, tag)
		}
		if r.LastGood != "" {
			got = append(got, "last-good:"+r.LastGood)
			if !r.Urgent {
				t.Fatal("license change backup should be urgent")
			}
		}
	}
	if want := []string{"v1.0.0", "v1.1.0", "last-good:v1.1.1"}; !slices.Equal(got, want) {
		t.Fatalf("backed up %v, want %v", got, want)
	}
	decisions, _ := s.Decisions(ctx, "up/app", 10)
	for _, d := range decisions {
		if d.Ref == "v2.0.0" && !slices.Equal(d.Pending, []string{"notify"}) {
			t.Fatalf("archive should be done: %+v", d)
		}
	}
}

type fakeNotifier struct{ notices []notify.Notice }

func (f *fakeNotifier) Notify(_ context.Context, n notify.Notice) (bool, error) {
	f.notices = append(f.notices, n)
	return true, nil
}

func TestApprovalHoldsLaterVersions(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Parse([]byte(`
credentials: {provider: env}
repos:
  - upstream: up/app
    fork: me/app
license:
  allowlist: [MIT, GPL-3.0-only]
  rules:
    - name: to-gpl
      from: [MIT]
      to: [GPL-*]
      actions: [require_approval, notify]
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	syncer := &fakeSyncer{}
	g := fakeGate{
		"v2": result("v1", []string{"MIT"}, []string{"GPL-3.0-only"}, "v2"),
		"v3": result("v2", []string{"GPL-3.0-only"}, []string{"GPL-3.0-only"}, "v3"),
	}
	p := New(cfg, g, rules.New(cfg), s, syncer, slog.New(slog.DiscardHandler))
	n := &fakeNotifier{}
	p.Notify = n

	out, err := p.Run(ctx, event("v2"))
	if err != nil || out.Approval == nil || out.Approval.Status != store.ApprovalPending || out.Approval.Kind != store.ApprovalNeeded {
		t.Fatalf("v2: %+v %v", out.Approval, err)
	}
	if syncer.reqs[0].Decision.MergePolicy != rules.MergeApproval {
		t.Fatalf("v2 policy = %s", syncer.reqs[0].Decision.MergePolicy)
	}
	if len(n.notices) != 1 || n.notices[0].DecisionID == 0 || n.notices[0].Message.Priority != notify.PriorityHigh ||
		!strings.Contains(n.notices[0].Message.Body, "forkkeeper approve") {
		t.Fatalf("notices = %+v", n.notices)
	}

	// v3 has no license change of its own, but carries v2's: it waits too.
	out, err = p.Run(ctx, event("v3"))
	if err != nil || out.Approval != nil || len(out.Waiting) != 1 || out.Waiting[0].Ref != "v2" {
		t.Fatalf("v3: %+v %v", out, err)
	}
	if d := syncer.reqs[1].Decision; d.MergePolicy != rules.MergeApproval || !strings.Contains(d.Reason, "v2") {
		t.Fatalf("v3 decision = %+v", d)
	}
	if len(n.notices) != 1 {
		t.Fatalf("v3 notified: %+v", n.notices[1:])
	}

	// Once v2 is approved, re-running it syncs with auto-merge.
	if _, err := s.ResolveApproval(ctx, out.Waiting[0].ID, store.Resolution{Approve: true, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(ctx, event("v2")); err != nil {
		t.Fatal(err)
	}
	if d := syncer.reqs[2].Decision; d.MergePolicy != rules.MergeAuto {
		t.Fatalf("approved v2 policy = %s", d.MergePolicy)
	}
}

func TestFreezeNotifiesUrgently(t *testing.T) {
	p, _ := setup(t, fakeGate{"v3": result("v2", []string{"MIT"}, []string{"BUSL-1.1"}, "v3")})
	n := &fakeNotifier{}
	p.Notify = n
	if _, err := p.Run(context.Background(), event("v3")); err != nil {
		t.Fatal(err)
	}
	if len(n.notices) != 1 || n.notices[0].Message.Priority != notify.PriorityUrgent ||
		!strings.HasPrefix(n.notices[0].Message.Title, "FROZEN") || !strings.Contains(n.notices[0].Message.Body, "frozen at v2") {
		t.Fatalf("notices = %+v", n.notices)
	}
}
