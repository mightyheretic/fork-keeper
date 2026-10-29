package merge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// ReconcileStore is the subset of the store the reconciler uses.
type ReconcileStore interface {
	OpenSyncs(ctx context.Context) ([]store.Sync, error)
	UpdateSyncStatus(ctx context.Context, id int64, status, detail string) error
}

// Reconciler follows open sync PRs: it notices when they are merged or
// closed, and merges PRs with an "auto" policy once CI passes.
type Reconciler struct {
	hosts map[string]provider.ForkHost
	store ReconcileStore
	log   *slog.Logger
	// Interval between checks.
	Interval time.Duration
	// AutoMerge enables merging "auto" PRs; RequireCI holds them until at
	// least one check has reported.
	AutoMerge bool
	RequireCI bool
}

// NewReconciler returns a reconciler with auto-merge on and CI required.
func NewReconciler(hosts map[string]provider.ForkHost, st ReconcileStore, log *slog.Logger) *Reconciler {
	return &Reconciler{hosts: hosts, store: st, log: log, Interval: 5 * time.Minute, AutoMerge: true, RequireCI: true}
}

// Run checks open PRs every Interval until ctx is done.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		if err := r.Once(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("checking sync PRs failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once checks every open sync PR.
func (r *Reconciler) Once(ctx context.Context) error {
	syncs, err := r.store.OpenSyncs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range syncs {
		if err := r.check(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("%s PR #%d: %w", s.Fork, s.PRNumber, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) check(ctx context.Context, s store.Sync) error {
	host, ok := r.hosts[s.Provider]
	if !ok || s.PRNumber == 0 {
		return nil
	}
	fork, err := provider.ParseRepo(s.Fork)
	if err != nil {
		return err
	}
	log := r.log.With("repo", s.Repo, "fork", s.Fork, "ref", s.Ref, "pr", s.PRNumber)
	setDetail := func(status, detail string) error {
		if status == s.Status && detail == s.Detail {
			return nil
		}
		return r.store.UpdateSyncStatus(ctx, s.ID, status, detail)
	}

	pr, err := host.GetPullRequest(ctx, fork, s.PRNumber)
	if errors.Is(err, provider.ErrRefNotFound) {
		return setDetail(store.SyncClosed, "PR no longer exists")
	}
	if err != nil {
		return err
	}
	switch {
	case pr.Merged:
		log.Info("sync PR merged")
		return setDetail(store.SyncMerged, "merged")
	case pr.State != "open":
		log.Info("sync PR closed without merging")
		return setDetail(store.SyncClosed, "closed without merging")
	}

	if !r.AutoMerge || s.MergePolicy != string(rules.MergeAuto) {
		return nil
	}
	switch {
	case pr.Draft:
		return setDetail(s.Status, "draft; not merging automatically")
	case slices.Contains(pr.Labels, LabelApproval) || slices.Contains(pr.Labels, LabelBlocked):
		// An earlier release on this PR needed approval; that still stands.
		return setDetail(s.Status, "labeled for approval; not merging automatically")
	case pr.HeadSHA != s.HeadSHA:
		return setDetail(s.Status, "staging branch was changed outside Fork Keeper; not merging automatically")
	}

	ci, err := host.CIStatus(ctx, fork, pr.HeadSHA)
	if err != nil {
		return err
	}
	switch ci.State {
	case provider.CIPending:
		return setDetail(s.Status, "waiting for CI")
	case provider.CIFailure:
		log.Warn("CI failed on sync PR; not merging", "failed", ci.Failed)
		return setDetail(s.Status, "CI failed: "+strings.Join(ci.Failed, ", "))
	case provider.CINone:
		if r.RequireCI {
			return setDetail(s.Status, "waiting for CI (no checks reported yet)")
		}
	}

	title := fmt.Sprintf("Merge upstream %s %s (#%d)", s.Repo, s.Ref, s.PRNumber)
	if err := host.MergePullRequest(ctx, fork, s.PRNumber, pr.HeadSHA, title); err != nil {
		return fmt.Errorf("auto-merge: %w", err)
	}
	detail := "auto-merged after CI passed"
	if ci.State == provider.CINone {
		detail = "auto-merged (no CI configured)"
	}
	log.Info("sync PR auto-merged", "checks", ci.Total)
	return setDetail(store.SyncMerged, detail)
}
