// Package approval resolves approvals: a person approves or rejects an
// upstream version whose license change needs a human, from the CLI or
// from the sync PR on GitHub. Approving makes the fork's sync PR mergeable
// again; rejecting closes it and freezes the fork at the last good version.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/merge"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// Store is the subset of the store the service uses.
type Store interface {
	Approval(ctx context.Context, id int64) (store.Approval, bool, error)
	ResolveApproval(ctx context.Context, id int64, r store.Resolution) (store.Approval, error)
	PendingApprovals(ctx context.Context, provider, repo string) ([]store.Approval, error)
	UnappliedApprovals(ctx context.Context) ([]store.Approval, error)
	MarkApprovalApplied(ctx context.Context, id int64) error
	OpenSyncFor(ctx context.Context, provider, repo string) (store.Sync, bool, error)
	SyncFor(ctx context.Context, key store.RefKey) (store.Sync, bool, error)
	SetSyncPolicy(ctx context.Context, id int64, policy, detail string) error
	UpdateSyncStatus(ctx context.Context, id int64, status, detail string) error
	RepoState(ctx context.Context, provider, repo string) (store.RepoState, error)
}

// Service approves and rejects.
type Service struct {
	store Store
	hosts map[string]provider.ForkHost
	log   *slog.Logger
	// AutoMerge tells PR comments whether Fork Keeper will merge an
	// approved PR itself.
	AutoMerge bool
}

// New returns a service. hosts may be empty: decisions are still recorded,
// and their effect on PRs is carried out by the service later.
func New(st Store, hosts map[string]provider.ForkHost, log *slog.Logger) *Service {
	return &Service{store: st, hosts: hosts, log: log, AutoMerge: true}
}

// Result is what a resolution did.
type Result struct {
	Approval store.Approval
	// Detail describes the effect on the fork, for the CLI.
	Detail string
	// Frozen is set when a rejection froze the repo.
	Frozen bool
}

// Approve approves a pending approval. Overriding block_auto_merge needs a
// reason. When no other version of the repo is waiting, the fork's sync PR
// is made mergeable: marked ready for review and its approval labels removed.
func (s *Service) Approve(ctx context.Context, id int64, actor, reason string) (Result, error) {
	a, ok, err := s.store.Approval(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, fmt.Errorf("no approval #%d", id)
	}
	if a.Kind == store.ApprovalOverride && strings.TrimSpace(reason) == "" {
		return Result{}, fmt.Errorf("approval #%d overrides block_auto_merge on %s %s; give a --reason, it is recorded",
			id, a.Repo, a.Ref)
	}
	if a, err = s.store.ResolveApproval(ctx, id, store.Resolution{Approve: true, Actor: actor, Reason: reason}); err != nil {
		return Result{}, err
	}
	res := Result{Approval: a}
	res.Detail, err = s.apply(ctx, a)
	return res, err
}

// Reject rejects a pending approval: the repo is frozen at the version
// before it, and the fork's sync PR is closed.
func (s *Service) Reject(ctx context.Context, id int64, actor, reason string) (Result, error) {
	if strings.TrimSpace(reason) == "" {
		return Result{}, errors.New("a rejection needs a --reason; it is recorded")
	}
	a, ok, err := s.store.Approval(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, fmt.Errorf("no approval #%d", id)
	}
	var dc notify.DecisionContext
	_ = json.Unmarshal(a.Context, &dc)
	before, err := s.store.RepoState(ctx, a.Provider, a.Repo)
	if err != nil {
		return Result{}, err
	}
	if a, err = s.store.ResolveApproval(ctx, id, store.Resolution{Actor: actor, Reason: reason, FreezeAt: dc.PreviousRef}); err != nil {
		return Result{}, err
	}
	res := Result{Approval: a, Frozen: !before.Frozen}
	at := dc.PreviousRef
	if at == "" {
		at = "no version (this was the first one seen)"
	}
	res.Detail = "frozen at " + at
	if before.Frozen {
		res.Detail = "already frozen at " + orDash(before.FrozenRef)
	}

	if synced, ok, err := s.store.SyncFor(ctx, a.RefKey); err != nil {
		return res, err
	} else if ok && synced.Status == store.SyncMerged {
		res.Detail += fmt.Sprintf("; %s was already merged into %s (PR #%d): revert it there by hand",
			a.Ref, synced.Fork, synced.PRNumber)
	}
	open, ok, err := s.store.OpenSyncFor(ctx, a.Provider, a.Repo)
	if err != nil || !ok {
		return res, err
	}
	host, fork, err := s.host(open)
	if err != nil {
		return res, err
	}
	body := fmt.Sprintf("Fork Keeper: %s was **rejected** by %s: %s\n\nThe fork is frozen at %s and won't sync "+
		"until someone runs `forkkeeper unfreeze`.", a.Ref, actor, reason, "`"+at+"`")
	if err := host.Comment(ctx, fork, open.PRNumber, body); err != nil {
		s.log.Warn("could not comment on PR", "fork", open.Fork, "pr", open.PRNumber, "err", err)
	}
	if err := host.ClosePullRequest(ctx, fork, open.PRNumber); err != nil {
		return res, fmt.Errorf("close PR #%d on %s: %w", open.PRNumber, open.Fork, err)
	}
	if err := s.store.UpdateSyncStatus(ctx, open.ID, store.SyncClosed, "rejected by "+actor); err != nil {
		return res, err
	}
	res.Detail += fmt.Sprintf("; closed PR #%d on %s", open.PRNumber, open.Fork)
	return res, nil
}

// ApplyPending carries out approvals whose effect on a PR failed earlier
// (GitHub was down, or the CLI had no credentials).
func (s *Service) ApplyPending(ctx context.Context) error {
	pending, err := s.store.UnappliedApprovals(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range pending {
		if _, err := s.apply(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("approval #%d: %w", a.ID, err))
		}
	}
	return errors.Join(errs...)
}

// CheckPR resolves approvals from the sync PR: merging it by hand approves
// every waiting version of the repo, and so does the "forkkeeper: approved"
// label. It reports whether it changed anything.
func (s *Service) CheckPR(ctx context.Context, sync store.Sync, pr *provider.PullRequest) (bool, error) {
	var how string
	switch {
	case pr.Merged:
		how = fmt.Sprintf("PR #%d merged on GitHub", pr.Number)
	case pr.State == "open" && hasLabel(pr.Labels, notify.LabelApproved):
		how = fmt.Sprintf("label %q on PR #%d", notify.LabelApproved, pr.Number)
	default:
		return false, nil
	}
	pending, err := s.store.PendingApprovals(ctx, sync.Provider, sync.Repo)
	if err != nil || len(pending) == 0 {
		return false, err
	}
	var last store.Approval
	for _, a := range pending {
		r, err := s.store.ResolveApproval(ctx, a.ID, store.Resolution{Approve: true, Actor: "github", Reason: how})
		if errors.Is(err, store.ErrNotPending) {
			continue
		}
		if err != nil {
			return true, err
		}
		s.log.Info("approved from GitHub", "repo", a.Repo, "ref", a.Ref, "approval", a.ID, "how", how)
		if pr.Merged {
			// Nothing left to do on a merged PR.
			if err := s.store.MarkApprovalApplied(ctx, a.ID); err != nil {
				return true, err
			}
			continue
		}
		last = r
	}
	if last.ID != 0 {
		if _, err := s.apply(ctx, last); err != nil {
			return true, err
		}
	}
	return true, nil
}

// apply makes the repo's open sync PR mergeable once nothing is waiting.
func (s *Service) apply(ctx context.Context, a store.Approval) (string, error) {
	done := func(detail string) (string, error) { return detail, s.store.MarkApprovalApplied(ctx, a.ID) }
	open, ok, err := s.store.OpenSyncFor(ctx, a.Provider, a.Repo)
	if err != nil {
		return "", err
	}
	if !ok {
		return done("no sync PR is open; the approval applies when this version (or a newer one) is synced")
	}
	pending, err := s.store.PendingApprovals(ctx, a.Provider, a.Repo)
	if err != nil {
		return "", err
	}
	if len(pending) > 0 {
		var waiting []string
		for _, p := range pending {
			waiting = append(waiting, fmt.Sprintf("#%d (%s)", p.ID, p.Ref))
		}
		return done(fmt.Sprintf("PR #%d stays on hold: still waiting on %s", open.PRNumber, strings.Join(waiting, ", ")))
	}
	state, err := s.store.RepoState(ctx, a.Provider, a.Repo)
	if err != nil {
		return "", err
	}
	if state.Frozen {
		return done(fmt.Sprintf("%s is frozen at %s; PR #%d stays on hold until it is unfrozen", a.Repo,
			orDash(state.FrozenRef), open.PRNumber))
	}
	host, fork, err := s.host(open)
	if err != nil {
		return "", err
	}
	if err := host.MarkReadyForReview(ctx, fork, open.PRNumber); err != nil {
		return "", fmt.Errorf("mark PR #%d ready: %w", open.PRNumber, err)
	}
	for _, l := range []string{merge.LabelApproval, merge.LabelBlocked} {
		if err := host.RemoveLabel(ctx, fork, open.PRNumber, l); err != nil {
			return "", fmt.Errorf("remove label from PR #%d: %w", open.PRNumber, err)
		}
	}
	next := "Merge it when you're ready."
	if s.AutoMerge {
		next = "Fork Keeper merges it once CI passes."
	}
	body := fmt.Sprintf("Fork Keeper: %s was **approved** by %s", a.Ref, a.ResolvedBy)
	if a.Resolution != "" {
		body += ": " + a.Resolution
	}
	body += ".\n\n" + next
	if err := host.Comment(ctx, fork, open.PRNumber, body); err != nil {
		s.log.Warn("could not comment on PR", "fork", open.Fork, "pr", open.PRNumber, "err", err)
	}
	if err := s.store.SetSyncPolicy(ctx, open.ID, string(rules.MergeAuto), "approved by "+a.ResolvedBy); err != nil {
		return "", err
	}
	return done(fmt.Sprintf("PR #%d on %s is ready: %s", open.PRNumber, open.Fork, strings.ToLower(next[:1])+next[1:]))
}

func (s *Service) host(sync store.Sync) (provider.ForkHost, provider.Repo, error) {
	host, ok := s.hosts[sync.Provider]
	if !ok {
		return nil, provider.Repo{}, fmt.Errorf("no %s access configured for %s", sync.Provider, sync.Fork)
	}
	fork, err := provider.ParseRepo(sync.Fork)
	return host, fork, err
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
