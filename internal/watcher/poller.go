// Package watcher polls upstream repos for new releases and tags.
//
// Webhooks only reach repos where the Fork Keeper App is installed, which
// normally excludes upstreams you don't own, so polling is the primary way
// upstream releases are discovered.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// Target is one upstream to poll.
type Target struct {
	Provider           string
	Repo               provider.Repo
	Kind               provider.EventKind // KindRelease or KindTag
	IncludePrereleases bool
}

// Store is the subset of the store the poller uses.
type Store interface {
	PollState(ctx context.Context, key store.PollKey) (store.PollState, error)
	RecordPollUnchanged(ctx context.Context, key store.PollKey) error
	RecordPollError(ctx context.Context, key store.PollKey, cause error) error
	RecordRefs(ctx context.Context, key store.PollKey, etag string, events []provider.Event,
		shouldQueue func(provider.Event) bool) (store.PollOutcome, error)
}

// Result is the outcome of polling one target.
type Result struct {
	Target      Target
	NotModified bool
	// Refs is what the provider returned, newest first (empty if NotModified).
	Refs      []provider.Ref
	Outcome   store.PollOutcome
	RateLimit provider.RateLimit
	Err       error
}

// Poller polls each target on an interval.
type Poller struct {
	store   Store
	listers map[string]provider.RefLister // by provider name
	targets []Target
	log     *slog.Logger
	// OnQueued is called after new events are queued, to wake the worker.
	OnQueued func()
	Interval time.Duration
	// Spacing is the pause between targets, to avoid bursts.
	Spacing time.Duration

	now         func() time.Time
	pausedUntil time.Time
}

// New returns a poller. listers maps a provider name to its RefLister.
func New(st Store, listers map[string]provider.RefLister, targets []Target, log *slog.Logger) *Poller {
	return &Poller{
		store:    st,
		listers:  listers,
		targets:  targets,
		log:      log,
		Interval: 15 * time.Minute,
		Spacing:  time.Second,
		now:      time.Now,
	}
}

// Run polls immediately and then every Interval until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	if len(p.targets) == 0 {
		p.log.Info("no repos configured; upstream polling is idle")
		return
	}
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		p.PollAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PollAll polls every target once and returns the results.
func (p *Poller) PollAll(ctx context.Context) []Result {
	results := make([]Result, 0, len(p.targets))
	for i, t := range p.targets {
		if ctx.Err() != nil {
			break
		}
		if i > 0 && p.Spacing > 0 {
			select {
			case <-ctx.Done():
				return results
			case <-time.After(p.Spacing):
			}
		}
		if wait := p.pausedUntil.Sub(p.now()); wait > 0 {
			err := fmt.Errorf("skipped: rate limited for another %s", wait.Round(time.Second))
			results = append(results, Result{Target: t, Err: err})
			continue
		}
		r := p.PollOne(ctx, t, false)
		results = append(results, r)
		p.logResult(r)
	}
	return results
}

// PollOne polls a single target. With dryRun, it lists refs without reading
// or changing any state.
func (p *Poller) PollOne(ctx context.Context, t Target, dryRun bool) Result {
	res := Result{Target: t}
	key := store.PollKey{Provider: t.Provider, Repo: t.Repo.FullName(), Kind: t.Kind}
	lister, ok := p.listers[t.Provider]
	if !ok {
		res.Err = fmt.Errorf("no lister for provider %q", t.Provider)
		return res
	}

	var list provider.RefList
	var err error
	switch t.Kind {
	case provider.KindRelease:
		etag := ""
		if !dryRun {
			st, err := p.store.PollState(ctx, key)
			if err != nil {
				res.Err = err
				return res
			}
			// Only trust the ETag after a baseline, so an interrupted first
			// poll is retried in full.
			if st.Baselined {
				etag = st.ETag
			}
		}
		list, err = lister.ListReleases(ctx, t.Repo, etag)
	case provider.KindTag:
		list, err = lister.ListTags(ctx, t.Repo)
	default:
		err = fmt.Errorf("unsupported watch kind %q", t.Kind)
	}
	res.RateLimit = list.RateLimit

	if err != nil {
		var rl *provider.RateLimitError
		if errors.As(err, &rl) && rl.Reset.After(p.pausedUntil) {
			p.pausedUntil = rl.Reset
		}
		res.Err = err
		if !dryRun {
			if recErr := p.store.RecordPollError(ctx, key, err); recErr != nil {
				p.log.Error("record poll error", "repo", key.Repo, "err", recErr)
			}
		}
		return res
	}
	res.NotModified = list.NotModified
	res.Refs = list.Refs
	if dryRun {
		return res
	}
	if list.NotModified {
		if err := p.store.RecordPollUnchanged(ctx, key); err != nil {
			res.Err = err
		}
		return res
	}

	// Record oldest first so queued events are processed in release order.
	events := make([]provider.Event, 0, len(list.Refs))
	for _, ref := range slices.Backward(list.Refs) {
		events = append(events, refEvent(t, ref, p.now()))
	}
	res.Outcome, res.Err = p.store.RecordRefs(ctx, key, list.ETag, events, func(ev provider.Event) bool {
		return t.IncludePrereleases || !ev.Prerelease
	})
	if res.Err == nil && len(res.Outcome.Queued) > 0 && p.OnQueued != nil {
		p.OnQueued()
	}
	return res
}

// refEvent builds the queued event for a newly seen ref. The delivery ID is
// derived from the ref, so polling the same ref again never queues it twice.
func refEvent(t Target, ref provider.Ref, now time.Time) provider.Event {
	return provider.Event{
		Provider:    t.Provider,
		DeliveryID:  fmt.Sprintf("poll:%s:%s:%s", ref.Kind, t.Repo.FullName(), ref.Name),
		Kind:        ref.Kind,
		Repo:        t.Repo,
		Tag:         ref.Name,
		ReleaseName: ref.Title,
		Prerelease:  ref.Prerelease,
		URL:         ref.URL,
		PublishedAt: ref.PublishedAt,
		Source:      provider.SourcePoll,
		ReceivedAt:  now.UTC(),
	}
}

func (p *Poller) logResult(r Result) {
	log := p.log.With("repo", r.Target.Repo.FullName(), "watch", r.Target.Kind)
	if r.RateLimit.Limit > 0 {
		log = log.With("rate_remaining", r.RateLimit.Remaining, "rate_limit", r.RateLimit.Limit)
	}
	switch {
	case r.Err != nil:
		log.Warn("poll failed", "err", r.Err)
	case r.NotModified:
		log.Debug("poll: not modified")
	case r.Outcome.Baseline:
		latest := ""
		if len(r.Refs) > 0 {
			latest = r.Refs[0].Name
		}
		log.Info("poll baseline recorded; future releases will be queued", "refs", len(r.Outcome.New), "latest", latest)
	default:
		for _, ev := range r.Outcome.Queued {
			log.Info("new upstream ref queued", "tag", ev.Tag, "prerelease", ev.Prerelease)
		}
		if skipped := len(r.Outcome.New) - len(r.Outcome.Queued); skipped > 0 {
			log.Info("new pre-releases seen but not queued (include_prereleases is off)", "count", skipped)
		}
	}
	if rl := r.RateLimit; rl.Limit > 0 && rl.Remaining < rl.Limit/10 {
		log.Warn("GitHub API quota is running low", "resets_at", rl.Reset)
	}
}
