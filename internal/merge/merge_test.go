package merge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/git"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// fakeHost serves repos from local directories and keeps PRs in memory.
type fakeHost struct {
	mu     sync.Mutex
	dirs   map[string]string // full name -> path
	prs    []*provider.PullRequest
	heads  map[int]string // PR number -> head branch
	bases  map[int]string
	bodies map[int]string
	ci     provider.CIStatus
	merged []int
}

func (h *fakeHost) CloneURL(repo provider.Repo) string { return h.dirs[repo.FullName()] }
func (h *fakeHost) GitAuth(context.Context, provider.Repo) (credentials.Secret, error) {
	return credentials.Secret{}, nil
}
func (h *fakeHost) DefaultBranch(context.Context, provider.Repo) (string, error) { return "main", nil }

func (h *fakeHost) FindPullRequest(_ context.Context, _ provider.Repo, head, base string) (*provider.PullRequest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, pr := range h.prs {
		if pr.State == "open" && h.heads[pr.Number] == head && h.bases[pr.Number] == base {
			return pr, nil
		}
	}
	return nil, nil
}

func (h *fakeHost) CreatePullRequest(_ context.Context, _ provider.Repo, in provider.PullRequestInput) (*provider.PullRequest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pr := &provider.PullRequest{Number: len(h.prs) + 1, State: "open", Draft: in.Draft}
	pr.URL = fmt.Sprintf("https://example.test/pull/%d", pr.Number)
	h.prs = append(h.prs, pr)
	h.heads[pr.Number], h.bases[pr.Number], h.bodies[pr.Number] = in.Head, in.Base, in.Body
	return pr, nil
}

func (h *fakeHost) UpdatePullRequest(_ context.Context, _ provider.Repo, n int, in provider.PullRequestInput) (*provider.PullRequest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies[n] = in.Body
	return h.prs[n-1], nil
}

func (h *fakeHost) GetPullRequest(_ context.Context, _ provider.Repo, n int) (*provider.PullRequest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := *h.prs[n-1]
	return &cp, nil
}

func (h *fakeHost) AddLabels(_ context.Context, _ provider.Repo, n int, labels []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	pr := h.prs[n-1]
	for _, l := range labels {
		if !slices.Contains(pr.Labels, l) {
			pr.Labels = append(pr.Labels, l)
		}
	}
	return nil
}

func (h *fakeHost) CIStatus(context.Context, provider.Repo, string) (provider.CIStatus, error) {
	return h.ci, nil
}

func (h *fakeHost) MergePullRequest(_ context.Context, _ provider.Repo, n int, sha, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.merged = append(h.merged, n)
	h.prs[n-1].Merged, h.prs[n-1].State = true, "closed"
	return nil
}

// sh runs git in dir for test setup.
func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.test",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", file)
	sh(t, dir, "commit", "-q", "-m", msg)
}

type env struct {
	t        *testing.T
	upstream string
	fork     string
	host     *fakeHost
	store    *store.Store
	engine   *Engine
}

// newEnv creates an upstream repo with tag v1 and a fork cloned from it.
func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	e := &env{t: t, upstream: filepath.Join(root, "upstream"), fork: filepath.Join(root, "fork")}
	os.Mkdir(e.upstream, 0o755)
	sh(t, e.upstream, "init", "-q", "-b", "main")
	commit(t, e.upstream, "a.txt", "a\n", "initial")
	commit(t, e.upstream, "LICENSE", "MIT\n", "license")
	sh(t, e.upstream, "tag", "v1")
	sh(t, root, "clone", "-q", e.upstream, e.fork)

	e.host = &fakeHost{dirs: map[string]string{"up/app": e.upstream, "me/app": e.fork},
		heads: map[int]string{}, bases: map[int]string{}, bodies: map[int]string{}}
	st, err := store.Open(context.Background(), filepath.Join(root, "db", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.store = st
	runner := &git.Runner{Home: filepath.Join(root, "home"), AllowFileProtocol: true,
		Identity: git.Identity{Name: "Fork Keeper", Email: "fk@example.test"}}
	os.Mkdir(runner.Home, 0o700)
	e.engine = New(map[string]provider.ForkHost{"github": e.host}, st, runner, filepath.Join(root, "git"),
		slog.New(slog.DiscardHandler))
	return e
}

func (e *env) sync(ref, prev string, policy rules.MergePolicy, strategy string) Result {
	e.t.Helper()
	res, err := e.engine.Sync(context.Background(), e.request(ref, prev, policy, strategy))
	if err != nil {
		e.t.Fatalf("sync %s: %v", ref, err)
	}
	return res
}

func (e *env) request(ref, prev string, policy rules.MergePolicy, strategy string) Request {
	if strategy == "" {
		strategy = config.StrategyMerge
	}
	return Request{
		Provider: "github", Upstream: provider.Repo{Owner: "up", Name: "app"}, Fork: provider.Repo{Owner: "me", Name: "app"},
		Ref: ref, PreviousRef: prev,
		Repo:     config.Repo{StagingBranch: config.DefaultStagingBranch, MergeStrategy: strategy},
		Decision: rules.Decision{MergePolicy: policy},
		License:  "Licenses unchanged (MIT).",
	}
}

// forkBranch returns the commit a fork branch points at, or "".
func (e *env) forkBranch(name string) string {
	out, err := exec.Command("git", "-C", e.fork, "rev-parse", "--verify", "-q", "refs/heads/"+name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestCleanMergeOpensPR(t *testing.T) {
	e := newEnv(t)
	commit(t, e.fork, "patch.txt", "local\n", "Add local patch")
	stable := sh(t, e.fork, "rev-parse", "HEAD")
	commit(t, e.upstream, "a.txt", "a2\n", "upstream change")
	sh(t, e.upstream, "tag", "v2")
	v2 := sh(t, e.upstream, "rev-parse", "v2")

	res := e.sync("v2", "v1", rules.MergeAuto, "")
	if res.Status != store.SyncPROpen || res.Strategy != config.StrategyMerge || res.StableBranch != "main" {
		t.Fatalf("result = %+v", res.Sync)
	}
	if len(res.Patches) != 1 || res.Patches[0].Subject != "Add local patch" {
		t.Fatalf("patches = %+v", res.Patches)
	}
	// The staging branch is a merge of stable and the upstream tag; stable is untouched.
	if got := e.forkBranch(config.DefaultStagingBranch); got != res.HeadSHA {
		t.Fatalf("staging = %q, want %q", got, res.HeadSHA)
	}
	if parents := sh(t, e.fork, "show", "-s", "--format=%P", res.HeadSHA); parents != stable+" "+v2 {
		t.Fatalf("merge parents = %q", parents)
	}
	if e.forkBranch("main") != stable {
		t.Fatal("stable branch was changed")
	}
	if content := sh(t, e.fork, "show", res.HeadSHA+":a.txt"); content != "a2" {
		t.Fatalf("a.txt = %q", content)
	}
	if res.PR == nil || res.PR.Draft || !slices.Equal(res.PR.Labels, []string{LabelSync}) {
		t.Fatalf("PR = %+v", res.PR)
	}
	if body := e.host.bodies[1]; !strings.Contains(body, "Merge policy: auto") || !strings.Contains(body, "Add local patch") {
		t.Fatalf("PR body:\n%s", body)
	}

	// Processing the same ref again (a retry) reuses the result.
	if again := e.sync("v2", "v1", rules.MergeAuto, ""); !again.Already || len(e.host.prs) != 1 {
		t.Fatalf("retry: %+v, %d PRs", again.Sync, len(e.host.prs))
	}
}

func TestUpToDate(t *testing.T) {
	e := newEnv(t)
	res := e.sync("v1", "", rules.MergeAuto, "")
	if res.Status != store.SyncUpToDate || len(e.host.prs) != 0 || e.forkBranch(config.DefaultStagingBranch) != "" {
		t.Fatalf("result = %+v", res.Sync)
	}
}

func TestFallsBackToPatchStack(t *testing.T) {
	e := newEnv(t)
	// The fork patches a.txt; upstream later takes the same patch and then
	// changes the line again. A merge conflicts (both sides changed the
	// line), but the patch stack sees the patch is already upstream.
	commit(t, e.fork, "a.txt", "b\n", "Fix a")
	commit(t, e.fork, "fork.txt", "mine\n", "Fork-only file")
	stable := sh(t, e.fork, "rev-parse", "HEAD")
	commit(t, e.upstream, "a.txt", "b\n", "Fix a (from fork)")
	commit(t, e.upstream, "a.txt", "c\n", "Improve a")
	sh(t, e.upstream, "tag", "v2")
	v2 := sh(t, e.upstream, "rev-parse", "v2")

	res := e.sync("v2", "v1", rules.MergeAuto, "")
	if res.Status != store.SyncPROpen || res.Strategy != config.StrategyPatchStack {
		t.Fatalf("result = %+v", res.Sync)
	}
	// "Fix a" is patch-identical to an upstream commit, so it isn't even
	// listed; "Fork-only file" is re-applied.
	if len(res.Patches) != 1 || res.Patches[0].Subject != "Fork-only file" || len(res.Dropped) != 0 {
		t.Fatalf("patches = %+v dropped = %+v", res.Patches, res.Dropped)
	}
	if parents := sh(t, e.fork, "show", "-s", "--format=%P", res.HeadSHA); parents != stable+" "+v2 {
		t.Fatalf("merge parents = %q", parents)
	}
	if a := sh(t, e.fork, "show", res.HeadSHA+":a.txt"); a != "c" {
		t.Fatalf("a.txt = %q", a)
	}
	if f := sh(t, e.fork, "show", res.HeadSHA+":fork.txt"); f != "mine" {
		t.Fatalf("fork.txt = %q", f)
	}
	// The patch layer is upstream v2 plus the re-applied patch, with the original author.
	layer := e.forkBranch(res.PatchBranch)
	if layer == "" || sh(t, e.fork, "rev-parse", layer+"^") != v2 {
		t.Fatalf("patch layer %s = %q", res.PatchBranch, layer)
	}
	if who := sh(t, e.fork, "show", "-s", "--format=%an|%cn|%B", layer); !strings.HasPrefix(who, "Dev|Fork Keeper|Fork-only file") ||
		!strings.Contains(who, "cherry picked from commit") {
		t.Fatalf("re-applied commit = %q", who)
	}
}

func TestMergeOnlyReportsConflict(t *testing.T) {
	e := newEnv(t)
	commit(t, e.fork, "a.txt", "b\n", "Fix a")
	commit(t, e.upstream, "a.txt", "b\n", "Fix a (from fork)")
	commit(t, e.upstream, "a.txt", "c\n", "Improve a")
	sh(t, e.upstream, "tag", "v2")
	res := e.sync("v2", "v1", rules.MergeAuto, config.StrategyMergeOnly)
	if res.Status != store.SyncConflict || !slices.Equal(res.Conflicts, []string{"a.txt"}) {
		t.Fatalf("result = %+v", res.Sync)
	}
}

func TestConflictPushesNothing(t *testing.T) {
	e := newEnv(t)
	commit(t, e.fork, "a.txt", "fork\n", "Fork edit")
	commit(t, e.upstream, "a.txt", "upstream\n", "Upstream edit")
	sh(t, e.upstream, "tag", "v2")

	res := e.sync("v2", "v1", rules.MergeAuto, "")
	if res.Status != store.SyncConflict || !slices.Equal(res.Conflicts, []string{"a.txt"}) ||
		!strings.Contains(res.Detail, "Fork edit") {
		t.Fatalf("result = %+v", res.Sync)
	}
	if e.forkBranch(config.DefaultStagingBranch) != "" || len(e.host.prs) != 0 {
		t.Fatal("a conflicting sync must not push or open a PR")
	}
	rec, ok, _ := e.store.SyncFor(context.Background(), store.RefKey{Provider: "github", Repo: "up/app", Ref: "v2"})
	if !ok || rec.Status != store.SyncConflict {
		t.Fatalf("recorded = %+v", rec)
	}
	// A conflict isn't final: processing the ref again retries the merge.
	if again := e.sync("v2", "v1", rules.MergeAuto, ""); again.Already {
		t.Fatal("conflict should be retried, not reused")
	}
}

func TestNewerReleaseUpdatesPRAndKeepsApproval(t *testing.T) {
	e := newEnv(t)
	commit(t, e.upstream, "a.txt", "a2\n", "two")
	sh(t, e.upstream, "tag", "v2")
	commit(t, e.upstream, "a.txt", "a3\n", "three")
	sh(t, e.upstream, "tag", "v3")
	ctx := context.Background()

	first := e.sync("v2", "v1", rules.MergeApproval, "")
	if !first.PR.Draft || !slices.Contains(first.PR.Labels, LabelApproval) {
		t.Fatalf("approval PR = %+v", first.PR)
	}
	second := e.sync("v3", "v2", rules.MergeAuto, "")
	if second.PRNumber != first.PRNumber || len(e.host.prs) != 1 {
		t.Fatalf("v3 should update PR #%d, got #%d (%d PRs)", first.PRNumber, second.PRNumber, len(e.host.prs))
	}
	// The approval label from v2 is still there; auto-merge must not happen.
	if !slices.Contains(e.host.prs[0].Labels, LabelApproval) {
		t.Fatal("approval label was removed")
	}
	old, _, _ := e.store.SyncFor(ctx, store.RefKey{Provider: "github", Repo: "up/app", Ref: "v2"})
	if old.Status != store.SyncSuperseded {
		t.Fatalf("v2 status = %s", old.Status)
	}

	// An older version than one already synced is skipped.
	if back := e.sync("v2", "v1", rules.MergeAuto, ""); back.Status != store.SyncSuperseded || !back.Already {
		t.Fatalf("v2 again = %+v", back.Sync)
	}
	commit(t, e.upstream, "b.txt", "backport\n", "backport")
	sh(t, e.upstream, "tag", "v2.0.1")
	if back := e.sync("v2.0.1", "v2", rules.MergeAuto, ""); back.Status != store.SyncSkipped {
		t.Fatalf("backport = %+v", back.Sync)
	}

	r := NewReconciler(map[string]provider.ForkHost{"github": e.host}, e.store, slog.New(slog.DiscardHandler))
	e.host.ci = provider.CIStatus{State: provider.CISuccess, Total: 1}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.host.merged) != 0 {
		t.Fatal("a PR labeled for approval was auto-merged")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	commit(t, e.upstream, "a.txt", "a2\n", "two")
	sh(t, e.upstream, "tag", "v2")
	req := e.request("v2", "v1", rules.MergeAuto, "")
	req.DryRun = true
	res, err := e.engine.Sync(context.Background(), req)
	if err != nil || res.Status != store.SyncPROpen || res.HeadSHA == "" {
		t.Fatalf("dry run = %+v, %v", res.Sync, err)
	}
	if e.forkBranch(config.DefaultStagingBranch) != "" || len(e.host.prs) != 0 {
		t.Fatal("dry run pushed or opened a PR")
	}
	if _, ok, _ := e.store.SyncFor(context.Background(), res.RefKey); ok {
		t.Fatal("dry run recorded a sync")
	}
}

func TestMissingTagAndUnrelatedHistory(t *testing.T) {
	e := newEnv(t)
	if _, err := e.engine.Sync(context.Background(), e.request("v9", "v1", rules.MergeAuto, "")); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("missing tag: %v", err)
	}
	if _, err := e.engine.Sync(context.Background(), e.request("--upload-pack=x", "", rules.MergeAuto, "")); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("hostile tag name: %v", err)
	}

	other := filepath.Join(t.TempDir(), "other")
	os.Mkdir(other, 0o755)
	sh(t, other, "init", "-q", "-b", "main")
	commit(t, other, "x.txt", "x\n", "unrelated")
	sh(t, other, "tag", "v2")
	e.host.dirs["up/app"] = other
	if _, err := e.engine.Sync(context.Background(), e.request("v2", "", rules.MergeAuto, "")); !errors.Is(err, ErrNoCommonHistory) {
		t.Fatalf("unrelated: %v", err)
	}
}

func TestReconcilerAutoMerge(t *testing.T) {
	e := newEnv(t)
	commit(t, e.upstream, "a.txt", "a2\n", "two")
	sh(t, e.upstream, "tag", "v2")
	res := e.sync("v2", "v1", rules.MergeAuto, "")
	ctx := context.Background()
	r := NewReconciler(map[string]provider.ForkHost{"github": e.host}, e.store, slog.New(slog.DiscardHandler))
	e.host.prs[0].HeadSHA = res.HeadSHA

	status := func() store.Sync {
		s, _, _ := e.store.SyncFor(ctx, res.RefKey)
		return s
	}
	for _, tc := range []struct {
		ci     provider.CIStatus
		detail string
	}{
		{provider.CIStatus{State: provider.CINone}, "waiting for CI (no checks reported yet)"},
		{provider.CIStatus{State: provider.CIPending, Total: 1}, "waiting for CI"},
		{provider.CIStatus{State: provider.CIFailure, Failed: []string{"test"}, Total: 1}, "CI failed: test"},
	} {
		e.host.ci = tc.ci
		if err := r.Once(ctx); err != nil {
			t.Fatal(err)
		}
		if s := status(); s.Status != store.SyncPROpen || s.Detail != tc.detail || len(e.host.merged) != 0 {
			t.Fatalf("CI %s: %+v merged=%v", tc.ci.State, s, e.host.merged)
		}
	}
	e.host.ci = provider.CIStatus{State: provider.CISuccess, Total: 2}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s := status(); s.Status != store.SyncMerged || !slices.Equal(e.host.merged, []int{1}) {
		t.Fatalf("after CI success: %+v merged=%v", s, e.host.merged)
	}
}

func TestReconcilerNoticesManualMergeAndClose(t *testing.T) {
	e := newEnv(t)
	commit(t, e.upstream, "a.txt", "a2\n", "two")
	sh(t, e.upstream, "tag", "v2")
	res := e.sync("v2", "v1", rules.MergeApproval, "")
	ctx := context.Background()
	r := NewReconciler(map[string]provider.ForkHost{"github": e.host}, e.store, slog.New(slog.DiscardHandler))
	e.host.ci = provider.CIStatus{State: provider.CISuccess, Total: 1}
	if err := r.Once(ctx); err != nil || len(e.host.merged) != 0 {
		t.Fatalf("approval PR must not be auto-merged: %v %v", err, e.host.merged)
	}
	e.host.prs[0].State = "closed"
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := e.store.SyncFor(ctx, res.RefKey); s.Status != store.SyncClosed {
		t.Fatalf("status = %s", s.Status)
	}
}
