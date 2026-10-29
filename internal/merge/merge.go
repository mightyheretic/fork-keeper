// Package merge brings a new upstream version into a fork: it merges the
// upstream tag into the fork's stable branch on a staging branch, keeping
// history, and opens (or updates) a pull request. Nothing is ever pushed to
// the stable branch itself.
//
// Merges are computed with `git merge-tree --write-tree` in a bare
// repository, so upstream files are never checked out and nothing from the
// upstream repository runs on the host.
package merge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/git"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// PR labels. Approval and blocked labels stop auto-merge; they are never
// removed by Fork Keeper, so a newer release can't clear an older concern.
const (
	LabelSync     = "forkkeeper"
	LabelApproval = "forkkeeper: needs approval"
	LabelBlocked  = "forkkeeper: blocked"
	LabelFlagged  = "forkkeeper: license flagged"
)

// Store is the subset of the store the engine uses.
type Store interface {
	SyncFor(ctx context.Context, key store.RefKey) (store.Sync, bool, error)
	SyncedRefs(ctx context.Context, provider, repo string) ([]string, error)
	SaveSync(ctx context.Context, s store.Sync) error
}

// Engine runs syncs.
type Engine struct {
	hosts map[string]provider.ForkHost // by provider name
	store Store
	git   *git.Runner
	dir   string
	log   *slog.Logger
}

// New returns an engine keeping its git data under dir.
func New(hosts map[string]provider.ForkHost, st Store, runner *git.Runner, dir string, log *slog.Logger) *Engine {
	return &Engine{hosts: hosts, store: st, git: runner, dir: dir, log: log}
}

// Request is one upstream version to bring into a fork.
type Request struct {
	Provider    string
	Upstream    provider.Repo
	Fork        provider.Repo
	Ref         string
	PreviousRef string
	ReleaseURL  string
	// Repo supplies the branches and strategy.
	Repo     config.Repo
	Decision rules.Decision
	// License is a one-line summary of the license check for the PR body.
	License string
	// DryRun computes the result locally but pushes nothing, opens no PR
	// and records nothing.
	DryRun bool
	// Force re-syncs a ref that was already synced, or is older than one.
	Force bool
}

// Result is what Sync did.
type Result struct {
	store.Sync
	// Dropped lists local patches left out because upstream already has them.
	Dropped []store.Patch
	// PatchBranch holds the re-applied patch layer (patch-stack strategy).
	PatchBranch string
	PR          *provider.PullRequest
	// Already is true when an earlier sync of this ref was reused.
	Already bool
}

// ErrNoCommonHistory means the fork and upstream share no commits, so
// there is nothing to merge into.
var ErrNoCommonHistory = errors.New("fork and upstream have no common history")

// Sync brings req.Ref into the fork.
func (e *Engine) Sync(ctx context.Context, req Request) (Result, error) {
	key := store.RefKey{Provider: req.Provider, Repo: req.Upstream.FullName(), Ref: req.Ref}
	res := Result{Sync: store.Sync{RefKey: key, Fork: req.Fork.FullName(), MergePolicy: string(req.Decision.MergePolicy),
		StagingBranch: req.Repo.StagingBranch}}
	host, ok := e.hosts[req.Provider]
	if !ok {
		return res, fmt.Errorf("no fork host for provider %q", req.Provider)
	}

	if !req.DryRun && !req.Force {
		if prev, ok, err := e.store.SyncFor(ctx, key); err != nil {
			return res, err
		} else if ok && prev.Status != store.SyncConflict && prev.Status != store.SyncSkipped {
			return Result{Sync: prev, Already: true}, nil
		}
	}
	if !req.Force {
		synced, err := e.store.SyncedRefs(ctx, req.Provider, key.Repo)
		if err != nil {
			return res, err
		}
		for _, r := range synced {
			if provider.CompareVersions(r, req.Ref) > 0 {
				res.Status = store.SyncSkipped
				res.Detail = fmt.Sprintf("older than %s, which was already synced", r)
				return res, e.save(ctx, req, res.Sync)
			}
		}
	}

	repoDir := filepath.Join(e.dir, req.Provider, req.Fork.Owner, req.Fork.Name+".git")
	unlock, err := git.Lock(ctx, repoDir+".lock")
	if err != nil {
		return res, err
	}
	defer unlock()
	w := &work{e: e, dir: repoDir, host: host, req: req}
	if err := w.prepare(ctx); err != nil {
		return res, err
	}
	if err := w.run(ctx, &res); err != nil {
		return res, err
	}
	if res.Status == store.SyncConflict || res.Status == store.SyncUpToDate || req.DryRun {
		return res, e.save(ctx, req, res.Sync)
	}

	if err := w.push(ctx, &res); err != nil {
		return res, err
	}
	if err := e.openPR(ctx, host, req, &res); err != nil {
		return res, err
	}
	return res, e.save(ctx, req, res.Sync)
}

func (e *Engine) save(ctx context.Context, req Request, s store.Sync) error {
	if req.DryRun {
		return nil
	}
	return e.store.SaveSync(ctx, s)
}

// work is one sync's git operations in the fork's bare cache repository.
type work struct {
	e        *Engine
	dir      string
	host     provider.ForkHost
	req      Request
	forkAuth credentials.Secret

	stable   string // branch name
	stableC  string // commit
	upstream string // commit of the new tag
	previous string // commit of the previous tag, if fetched
	// patchHead is the re-applied patch layer (patch-stack strategy).
	patchHead string
}

func (w *work) git(ctx context.Context, args ...string) (string, error) {
	out, err := w.e.git.Run(ctx, git.Cmd{Dir: w.dir, Args: args})
	return strings.TrimSpace(out), err
}

const (
	forkRefs     = "refs/forkkeeper/fork/"
	upstreamRefs = "refs/forkkeeper/upstream/tags/"
)

// prepare fetches the fork's stable branch and the upstream tags.
func (w *work) prepare(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(w.dir, "HEAD")); err != nil {
		if err := os.MkdirAll(w.dir, 0o700); err != nil {
			return err
		}
		if _, err := w.git(ctx, "init", "--bare", "-q"); err != nil {
			return err
		}
	}
	for _, ref := range []string{w.req.Ref, w.req.PreviousRef} {
		if ref == "" {
			continue
		}
		if _, err := w.git(ctx, "check-ref-format", "refs/tags/"+ref); err != nil {
			return fmt.Errorf("%w: %q is not a valid tag name", provider.ErrRefNotFound, ref)
		}
	}

	w.stable = w.req.Repo.StableBranch
	if w.stable == "" {
		b, err := w.host.DefaultBranch(ctx, w.req.Fork)
		if err != nil {
			return fmt.Errorf("default branch of %s: %w", w.req.Fork.FullName(), err)
		}
		w.stable = b
	}
	if w.stable == w.req.Repo.StagingBranch {
		return errors.New("staging branch is the fork's stable branch; set stable_branch or staging_branch")
	}

	var err error
	if w.forkAuth, err = w.host.GitAuth(ctx, w.req.Fork); err != nil {
		return err
	}
	if err := w.fetch(ctx, w.req.Fork, w.forkAuth, "+refs/heads/"+w.stable+":"+forkRefs+w.stable); err != nil {
		return fmt.Errorf("fetch %s %s: %w", w.req.Fork.FullName(), w.stable, err)
	}
	upAuth, err := w.host.GitAuth(ctx, w.req.Upstream)
	if err != nil {
		return err
	}
	if err := w.fetch(ctx, w.req.Upstream, upAuth, "+refs/tags/"+w.req.Ref+":"+upstreamRefs+w.req.Ref); err != nil {
		return fmt.Errorf("fetch %s %s: %w", w.req.Upstream.FullName(), w.req.Ref, err)
	}
	if w.req.PreviousRef != "" {
		// Only used to tell upstream commits from the fork's own; a
		// deleted previous tag isn't fatal.
		if err := w.fetch(ctx, w.req.Upstream, upAuth, "+refs/tags/"+w.req.PreviousRef+":"+upstreamRefs+w.req.PreviousRef); err != nil {
			w.e.log.Warn("could not fetch previous upstream tag", "ref", w.req.PreviousRef, "err", err)
		} else if w.previous, err = w.git(ctx, "rev-parse", "--verify", upstreamRefs+w.req.PreviousRef+"^{commit}"); err != nil {
			return err
		}
	}
	if w.stableC, err = w.git(ctx, "rev-parse", "--verify", forkRefs+w.stable+"^{commit}"); err != nil {
		return err
	}
	if w.upstream, err = w.git(ctx, "rev-parse", "--verify", upstreamRefs+w.req.Ref+"^{commit}"); err != nil {
		return err
	}
	return nil
}

func (w *work) fetch(ctx context.Context, repo provider.Repo, auth credentials.Secret, refspec string) error {
	_, err := w.e.git.Run(ctx, git.Cmd{Dir: w.dir, Auth: auth,
		Args: []string{"fetch", "--no-tags", "--no-write-fetch-head", "--quiet", w.host.CloneURL(repo), refspec}})
	var gerr *git.Error
	if errors.As(err, &gerr) && strings.Contains(gerr.Stderr, "couldn't find remote ref") {
		return fmt.Errorf("%w: %w", provider.ErrRefNotFound, err)
	}
	return err
}

// run computes the staging commit, filling in res.
func (w *work) run(ctx context.Context, res *Result) error {
	res.StableBranch = w.stable
	if _, err := w.git(ctx, "merge-base", "--is-ancestor", w.upstream, w.stableC); err == nil {
		res.Status = store.SyncUpToDate
		res.Detail = fmt.Sprintf("%s already contains %s", w.stable, w.req.Ref)
		return nil
	}
	if _, err := w.git(ctx, "merge-base", w.upstream, w.stableC); err != nil {
		return ErrNoCommonHistory
	}
	patches, err := w.localPatches(ctx)
	if err != nil {
		return err
	}
	res.Patches = patches

	strategy := w.req.Repo.MergeStrategy
	if strategy != config.StrategyPatchStack {
		head, conflicts, err := w.merge(ctx)
		if err != nil {
			return err
		}
		if len(conflicts) == 0 {
			res.Strategy, res.HeadSHA = config.StrategyMerge, head
			res.Status = store.SyncPROpen
			return nil
		}
		if strategy == config.StrategyMergeOnly {
			res.Strategy, res.Status, res.Conflicts = config.StrategyMerge, store.SyncConflict, conflicts
			res.Detail = fmt.Sprintf("merging %s into %s conflicts in %d file(s)", w.req.Ref, w.stable, len(conflicts))
			return nil
		}
		w.e.log.Info("merge conflicts; re-applying local patches instead", "repo", w.req.Fork.FullName(),
			"ref", w.req.Ref, "conflicts", len(conflicts))
	}

	res.Strategy = config.StrategyPatchStack
	st, err := w.patchStack(ctx, patches)
	if err != nil {
		return err
	}
	res.Dropped = st.dropped
	if st.conflicts != nil {
		res.Status, res.Conflicts = store.SyncConflict, st.conflicts
		res.Detail = fmt.Sprintf("local patch %s (%s) conflicts with %s in %d file(s)",
			short(st.failed.SHA), st.failed.Subject, w.req.Ref, len(st.conflicts))
		if strategy == config.StrategyMerge {
			res.Detail = "merge and patch stack both conflict; " + res.Detail
		}
		return nil
	}
	head, err := w.stackMerge(ctx, st.head)
	if err != nil {
		return err
	}
	res.HeadSHA, res.Status = head, store.SyncPROpen
	res.PatchBranch = w.req.Repo.StagingBranch + "-patches"
	w.patchHead = st.head
	return nil
}

// localPatches lists the fork's own commits: on the stable branch, not in
// the new upstream tag (or the previous one), and with no equivalent
// upstream commit. Oldest first.
func (w *work) localPatches(ctx context.Context) ([]store.Patch, error) {
	args := []string{"log", "--format=%H%x1f%s", "--reverse", "--no-merges", "--cherry-pick", "--right-only",
		w.upstream + "..." + w.stableC}
	if w.previous != "" {
		args = append(args, "^"+w.previous)
	}
	lines, err := w.e.git.Lines(ctx, git.Cmd{Dir: w.dir, Args: args})
	if err != nil {
		return nil, err
	}
	patches := make([]store.Patch, 0, len(lines))
	for _, l := range lines {
		sha, subject, _ := strings.Cut(l, "\x1f")
		patches = append(patches, store.Patch{SHA: sha, Subject: subject})
	}
	return patches, nil
}

// mergeTree merges two commits without a worktree. It returns the
// resulting tree and any conflicted paths.
func (w *work) mergeTree(ctx context.Context, base, ours, theirs string) (string, []string, error) {
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages"}
	if base != "" {
		args = append(args, "--merge-base="+base)
	}
	out, err := w.e.git.Run(ctx, git.Cmd{Dir: w.dir, Args: append(args, ours, theirs)})
	var gerr *git.Error
	if err != nil && !(errors.As(err, &gerr) && gerr.ExitCode == 1) {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	tree := lines[0]
	if err == nil {
		return tree, nil, nil
	}
	var conflicts []string
	for _, l := range lines[1:] {
		if l != "" {
			conflicts = append(conflicts, l)
		}
	}
	if len(conflicts) == 0 {
		conflicts = []string{"(unknown)"}
	}
	return tree, conflicts, nil
}

func (w *work) commitTree(ctx context.Context, tree, message string, env []string, parents ...string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := w.e.git.Run(ctx, git.Cmd{Dir: w.dir, Args: append(args, "-F", "-"), Stdin: message, Env: env})
	return strings.TrimSpace(out), err
}

// merge merges the upstream tag into stable, preserving history.
func (w *work) merge(ctx context.Context) (string, []string, error) {
	tree, conflicts, err := w.mergeTree(ctx, "", w.stableC, w.upstream)
	if err != nil || len(conflicts) > 0 {
		return "", conflicts, err
	}
	msg := fmt.Sprintf("Merge upstream %s %s into %s\n\nSynced by Fork Keeper from %s.\n",
		w.req.Upstream.FullName(), w.req.Ref, w.stable, w.host.CloneURL(w.req.Upstream))
	head, err := w.commitTree(ctx, tree, msg, nil, w.stableC, w.upstream)
	return head, nil, err
}

type stack struct {
	head      string
	dropped   []store.Patch
	conflicts []string
	failed    store.Patch
}

// patchStack re-applies patches on top of the upstream tag, one
// cherry-pick at a time. Original authors are kept.
func (w *work) patchStack(ctx context.Context, patches []store.Patch) (stack, error) {
	s := stack{head: w.upstream}
	for _, p := range patches {
		info, err := w.git(ctx, "show", "-s", "--format=%P%x00%an%x00%ae%x00%ad%x00%T", "--date=raw", p.SHA)
		if err != nil {
			return s, err
		}
		f := strings.Split(info, "\x00")
		if len(f) != 5 || f[0] == "" {
			return s, fmt.Errorf("local patch %s has no parent", short(p.SHA))
		}
		parent := strings.Fields(f[0])[0]
		tree, conflicts, err := w.mergeTree(ctx, parent, s.head, p.SHA)
		if err != nil {
			return s, err
		}
		if len(conflicts) > 0 {
			s.conflicts, s.failed = conflicts, p
			return s, nil
		}
		if cur, err := w.git(ctx, "rev-parse", s.head+"^{tree}"); err != nil {
			return s, err
		} else if cur == tree {
			s.dropped = append(s.dropped, p) // upstream already has this change
			continue
		}
		msg, err := w.git(ctx, "show", "-s", "--format=%B", p.SHA)
		if err != nil {
			return s, err
		}
		env := []string{"GIT_AUTHOR_NAME=" + f[1], "GIT_AUTHOR_EMAIL=" + f[2], "GIT_AUTHOR_DATE=" + f[3]}
		if s.head, err = w.commitTree(ctx, tree, msg+"\n\n(cherry picked from commit "+p.SHA+")\n", env, s.head); err != nil {
			return s, err
		}
	}
	return s, nil
}

// stackMerge records the patch stack's result as a merge of stable and
// upstream, so the PR merges cleanly into stable without rewriting it.
func (w *work) stackMerge(ctx context.Context, stackHead string) (string, error) {
	tree, err := w.git(ctx, "rev-parse", stackHead+"^{tree}")
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Merge upstream %s %s into %s (patch stack)\n\n"+
		"Merging directly conflicted, so the fork's local patches were re-applied on top of\n"+
		"%s and this commit records the result as a merge. The re-applied patches are\n"+
		"on %s-patches.\n",
		w.req.Upstream.FullName(), w.req.Ref, w.stable, w.req.Ref, w.req.Repo.StagingBranch)
	return w.commitTree(ctx, tree, msg, nil, w.stableC, w.upstream)
}

// push force-pushes the staging branch (and patch layer). Fork Keeper owns
// these branches; the stable branch is never pushed.
func (w *work) push(ctx context.Context, res *Result) error {
	specs := []string{"+" + res.HeadSHA + ":refs/heads/" + w.req.Repo.StagingBranch}
	if res.PatchBranch != "" {
		specs = append(specs, "+"+w.patchHead+":refs/heads/"+res.PatchBranch)
	}
	_, err := w.e.git.Run(ctx, git.Cmd{Dir: w.dir, Auth: w.forkAuth,
		Args: append([]string{"push", "--quiet", "--no-verify", w.host.CloneURL(w.req.Fork)}, specs...)})
	var gerr *git.Error
	if errors.As(err, &gerr) && strings.Contains(gerr.Stderr, "workflow") && strings.Contains(gerr.Stderr, "scope") ||
		errors.As(err, &gerr) && strings.Contains(gerr.Stderr, "without `workflows` permission") {
		return fmt.Errorf("push to %s rejected: upstream changed GitHub Actions workflows, which needs the "+
			"workflows permission (App: Workflows read & write; PAT: Workflows read & write): %w", w.req.Fork.FullName(), err)
	}
	if err != nil {
		return fmt.Errorf("push to %s: %w", w.req.Fork.FullName(), err)
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
