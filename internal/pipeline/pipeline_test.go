package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/gate"
	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/merge"
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
