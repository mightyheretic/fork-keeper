// Package pipeline processes a queued upstream release: license gate, then
// rule engine, then the actions that can be applied right away, then the
// source backup, then the merge engine. Later stages (notify) pick up the
// recorded pending actions.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/archive"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/gate"
	"github.com/mightyheretic/fork-keeper/internal/merge"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/worker"
)

// Store is the subset of the store the pipeline uses.
type Store interface {
	SaveDecision(ctx context.Context, d store.Decision) (int64, error)
	Freeze(ctx context.Context, provider, repo, ref, reason string, decisionID int64) (bool, error)
	RepoState(ctx context.Context, provider, repo string) (store.RepoState, error)
	CompletePending(ctx context.Context, decisionID int64, action string) error
	SaveApproval(ctx context.Context, key store.RefKey, decisionID int64, kind string) (store.Approval, error)
	PendingApprovals(ctx context.Context, provider, repo string) ([]store.Approval, error)
}

// Notifier queues notifications.
type Notifier interface {
	Notify(ctx context.Context, n notify.Notice) (bool, error)
}

// Checker runs the license gate.
type Checker interface {
	Check(ctx context.Context, ev provider.Event) (gate.Result, error)
}

// Syncer runs the merge engine.
type Syncer interface {
	Sync(ctx context.Context, req merge.Request) (merge.Result, error)
}

// Archiver makes source backups.
type Archiver interface {
	Backup(ctx context.Context, req archive.Request) (archive.Result, error)
}

// Pipeline handles one event at a time.
type Pipeline struct {
	cfg    *config.Config
	gate   Checker
	engine *rules.Engine
	store  Store
	syncer Syncer
	log    *slog.Logger
	// ForceSync re-syncs refs that were already synced (CLI use).
	ForceSync bool
	// Archiver backs up releases; nil disables backups.
	Archiver Archiver
	// Notify queues notifications; nil disables them.
	Notify Notifier
}

// New returns a pipeline. syncer may be nil to only check licenses.
func New(cfg *config.Config, g Checker, engine *rules.Engine, st Store, syncer Syncer, log *slog.Logger) *Pipeline {
	return &Pipeline{cfg: cfg, gate: g, engine: engine, store: st, syncer: syncer, log: log}
}

// Outcome is what Process did, for logging and tests.
type Outcome struct {
	Skipped  string // non-empty when the event was ignored, with the reason
	Check    gate.Result
	Decision rules.Decision
	// NewlyFrozen is true when this event froze the repo.
	NewlyFrozen bool
	State       store.RepoState
	// Approval is set when the version needs a human decision.
	Approval *store.Approval
	// Waiting lists earlier versions still waiting for approval; until
	// they are approved, this version needs approval too.
	Waiting []store.Approval
	// Backup is set when a source backup ran.
	Backup *archive.Result
	// Sync is set when the merge engine ran.
	Sync *merge.Result
}

// Process is a worker.Processor.
func (p *Pipeline) Process(ctx context.Context, ev provider.Event) error {
	_, err := p.Run(ctx, ev)
	return err
}

// Run processes ev and reports the outcome.
func (p *Pipeline) Run(ctx context.Context, ev provider.Event) (Outcome, error) {
	log := p.log.With("repo", ev.Repo.FullName(), "tag", ev.Tag, "source", ev.Source, "delivery", ev.DeliveryID)
	var out Outcome
	if ev.Kind != provider.KindRelease && ev.Kind != provider.KindTag {
		out.Skipped = "not a release or tag"
		return out, nil
	}
	repoCfg, ok := p.cfg.RepoFor(ev.Repo.FullName())
	if !ok {
		out.Skipped = "not a configured upstream"
		log.Info("ignoring event for a repo that isn't a configured upstream")
		return out, nil
	}

	res, err := p.gate.Check(ctx, ev)
	if errors.Is(err, provider.ErrRefNotFound) {
		return out, worker.Permanent(err)
	}
	if err != nil {
		return out, err
	}
	out.Check = res

	repo := ev.Repo.FullName()
	if res.Diff == nil {
		out.Decision = p.engine.EvaluateFirst(repo, res.Report)
	} else {
		out.Decision = p.engine.Evaluate(repo, res.Diff)
	}
	d := out.Decision

	// freeze is applied here; allow needs nothing. Everything else is
	// carried out by later stages.
	pending := slices.DeleteFunc(slices.Clone(d.Actions), func(a string) bool {
		return a == config.ActionFreeze || a == config.ActionAllow
	})
	id, err := p.store.SaveDecision(ctx, store.Decision{
		RefKey:      store.RefKey{Provider: ev.Provider, Repo: repo, Ref: ev.Tag},
		PreviousRef: res.Previous,
		Rule:        d.Rule,
		Scope:       d.Scope,
		Actions:     d.Actions,
		MergePolicy: string(d.MergePolicy),
		Reason:      d.Reason,
		Matched:     d.Matched,
		Pending:     pending,
	})
	if err != nil {
		return out, err
	}
	key := store.RefKey{Provider: ev.Provider, Repo: repo, Ref: ev.Tag}
	if kind := approvalKind(d); kind != "" {
		a, err := p.store.SaveApproval(ctx, key, id, kind)
		if err != nil {
			return out, err
		}
		out.Approval = &a
	}
	pendingApprovals, err := p.store.PendingApprovals(ctx, ev.Provider, repo)
	if err != nil {
		return out, err
	}
	for _, a := range pendingApprovals {
		if a.Ref != ev.Tag {
			out.Waiting = append(out.Waiting, a)
		}
	}

	if d.Has(config.ActionFreeze) {
		// The last good version is the one before this change. For a
		// first version there is none: the fork must not sync at all.
		if out.NewlyFrozen, err = p.store.Freeze(ctx, ev.Provider, repo, res.Previous, d.Reason, id); err != nil {
			return out, err
		}
	}
	if out.State, err = p.store.RepoState(ctx, ev.Provider, repo); err != nil {
		return out, err
	}

	attrs := []any{"rule", d.Rule, "actions", d.Actions, "merge_policy", d.MergePolicy, "reason", d.Reason}
	if res.Previous != "" {
		attrs = append(attrs, "previous", res.Previous)
	}
	switch {
	case out.NewlyFrozen:
		at := out.State.FrozenRef
		if at == "" {
			at = "(no version synced)"
		}
		log.Warn("REPO FROZEN: license change", append(attrs, "frozen_at", at)...)
	case out.State.Frozen:
		log.Warn("repo is frozen; new version recorded but will not be synced",
			append(attrs, "frozen_at", out.State.FrozenRef, "frozen_since", out.State.FrozenAt)...)
	case d.Rule == "":
		log.Info("license unchanged", "previous", res.Previous, "declared", res.Report.Declared)
	default:
		log.Warn("license rule fired", attrs...)
	}
	if len(d.Matched) > 1 {
		log.Info("other rules also matched but had lower precedence", "rules", strings.Join(d.Matched[1:], ", "))
	}

	// Back up before syncing: protecting the source matters most. A
	// failed backup is retried with the event; the sync is idempotent.
	backupErr := p.backup(ctx, log, ev, repoCfg, res, id, &out)
	syncErr := p.sync(ctx, log, ev, repoCfg, res, &out)
	p.notify(ctx, log, ev, id, &out)
	return out, errors.Join(backupErr, syncErr)
}

func (p *Pipeline) sync(ctx context.Context, log *slog.Logger, ev provider.Event, repoCfg config.Repo, res gate.Result,
	out *Outcome) error {
	if repoCfg.Fork == "" || p.syncer == nil {
		return nil
	}
	if out.State.Frozen {
		log.Info("not syncing the fork: repo is frozen", "frozen_at", out.State.FrozenRef)
		return nil
	}
	fork, err := provider.ParseRepo(repoCfg.Fork)
	if err != nil {
		return worker.Permanent(err)
	}
	req := SyncRequest(ev, fork, repoCfg, res, effectiveDecision(out))
	req.Force = p.ForceSync
	sync, err := p.syncer.Sync(ctx, req)
	if errors.Is(err, provider.ErrRefNotFound) || errors.Is(err, merge.ErrNoCommonHistory) {
		return worker.Permanent(err)
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", repoCfg.Fork, err)
	}
	out.Sync = &sync
	logSync(log, sync)
	return nil
}

// approvalKind is the kind of approval a decision needs, or "".
func approvalKind(d rules.Decision) string {
	switch d.MergePolicy {
	case rules.MergeBlocked:
		return store.ApprovalOverride
	case rules.MergeApproval:
		return store.ApprovalNeeded
	}
	return ""
}

// effectiveDecision is the decision the merge engine works with. An
// approved version merges like any other; a version that arrives while an
// earlier one is still waiting for approval waits too, since it carries
// the same change.
func effectiveDecision(out *Outcome) rules.Decision {
	d := out.Decision
	if a := out.Approval; a != nil && a.Status == store.ApprovalApproved {
		d.MergePolicy = rules.MergeAuto
	}
	for _, w := range out.Waiting {
		policy := rules.MergeApproval
		if w.Kind == store.ApprovalOverride {
			policy = rules.MergeBlocked
		}
		if d.MergePolicy == rules.MergeAuto || policy == rules.MergeBlocked && d.MergePolicy == rules.MergeApproval {
			d.MergePolicy = policy
		}
	}
	if d.MergePolicy != out.Decision.MergePolicy && len(out.Waiting) > 0 && d.Rule == "" {
		d.Reason = fmt.Sprintf("an earlier version (%s) is still waiting for approval #%d", out.Waiting[0].Ref, out.Waiting[0].ID)
	}
	return d
}

// backup backs up the source of the new release if the archive policy
// wants it and the repo isn't frozen. When a rule froze the repo or asked
// for an archive, it backs up the previous version instead, as the last
// good one, whatever the policy.
func (p *Pipeline) backup(ctx context.Context, log *slog.Logger, ev provider.Event, repoCfg config.Repo,
	res gate.Result, decisionID int64, out *Outcome) error {
	if p.Archiver == nil {
		return nil
	}
	d := out.Decision
	settings := p.cfg.ArchiveFor(repoCfg)
	req := archive.Request{Provider: ev.Provider, Upstream: ev.Repo, Fork: repoCfg.Fork, Settings: settings}
	archiveAction := d.Has(config.ActionArchive)
	switch {
	case out.NewlyFrozen || archiveAction:
		if res.Previous == "" {
			log.Warn("license rule asks to archive the last good version, but there is no earlier version")
			break
		}
		req.LastGood, req.Urgent = res.Previous, true
		req.Reason = fmt.Sprintf("last version before %s (%s)", ev.Tag, d.Rule)
	case out.State.Frozen:
	case !ev.Prerelease && archive.Wants(settings.Releases, ev.Tag):
		req.Tags = []string{ev.Tag}
		req.Reason = settings.Releases + " release"
	}
	if req.LastGood == "" && len(req.Tags) == 0 {
		return nil
	}
	r, err := p.Archiver.Backup(ctx, req)
	if err != nil {
		return fmt.Errorf("back up %s: %w", ev.Repo.FullName(), err)
	}
	out.Backup = &r
	log.Info("source backed up", "added", r.Added, "already_backed_up", r.Existing, "missing", r.Missing)
	if archiveAction && req.LastGood != "" {
		return p.store.CompletePending(ctx, decisionID, config.ActionArchive)
	}
	return nil
}

// SyncRequest builds the merge engine request for an event.
func SyncRequest(ev provider.Event, fork provider.Repo, repoCfg config.Repo, res gate.Result, d rules.Decision) merge.Request {
	summary := ""
	switch {
	case res.Diff != nil:
		summary = res.Diff.Summary()
	case res.Report != nil:
		summary = fmt.Sprintf("First version checked: %s is %s.", ev.Tag, res.Report.Declared)
	}
	return merge.Request{
		Provider:    ev.Provider,
		Upstream:    ev.Repo,
		Fork:        fork,
		Ref:         ev.Tag,
		PreviousRef: res.Previous,
		ReleaseURL:  ev.URL,
		Repo:        repoCfg,
		Decision:    d,
		License:     summary,
	}
}

func logSync(log *slog.Logger, s merge.Result) {
	attrs := []any{"fork", s.Fork, "status", s.Status, "strategy", s.Strategy, "local_patches", len(s.Patches)}
	if s.PRURL != "" {
		attrs = append(attrs, "pr", s.PRURL)
	}
	if s.Detail != "" {
		attrs = append(attrs, "detail", s.Detail)
	}
	switch {
	case s.Already:
		log.Info("fork already synced to this version", attrs...)
	case s.Status == store.SyncConflict:
		log.Warn("fork sync conflicts; needs manual merge", append(attrs, "conflicts", s.Conflicts)...)
	default:
		log.Info("fork sync done", attrs...)
	}
}
