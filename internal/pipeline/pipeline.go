// Package pipeline processes a queued upstream release: license gate, then
// rule engine, then the actions that can be applied right away. Later
// stages (merge, archive, notify) pick up the recorded pending actions.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/gate"
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
}

// Checker runs the license gate.
type Checker interface {
	Check(ctx context.Context, ev provider.Event) (gate.Result, error)
}

// Pipeline handles one event at a time.
type Pipeline struct {
	cfg    *config.Config
	gate   Checker
	engine *rules.Engine
	store  Store
	log    *slog.Logger
}

// New returns a pipeline.
func New(cfg *config.Config, g Checker, engine *rules.Engine, st Store, log *slog.Logger) *Pipeline {
	return &Pipeline{cfg: cfg, gate: g, engine: engine, store: st, log: log}
}

// Outcome is what Process did, for logging and tests.
type Outcome struct {
	Skipped  string // non-empty when the event was ignored, with the reason
	Check    gate.Result
	Decision rules.Decision
	// NewlyFrozen is true when this event froze the repo.
	NewlyFrozen bool
	State       store.RepoState
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
	if _, ok := p.cfg.RepoFor(ev.Repo.FullName()); !ok {
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
	return out, nil
}
