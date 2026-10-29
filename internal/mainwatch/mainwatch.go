// Package mainwatch is the default-branch early warning. Relicensing
// usually lands on an upstream's default branch weeks or months before a
// release ships it, so each upstream's default branch is checked between
// releases. Nothing from it is merged: only the top-level license (root
// license files and manifests) is read, through the provider API, and only
// when the head moved and those files changed.
//
// When the top-level license changes, the license rules run early (a
// freeze takes effect before the release arrives), and the last commit
// before the change is found and backed up as forkkeeper/last-open.
package mainwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/archive"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// Store is the subset of the store the watcher uses.
type Store interface {
	MainWatch(ctx context.Context, provider, repo string) (store.MainWatch, error)
	SaveMainWatch(ctx context.Context, w store.MainWatch) error
	RecordMainWatchError(ctx context.Context, provider, repo string, cause error) error
	LatestRef(ctx context.Context, provider, repo string, kind provider.EventKind) (string, bool, error)
	SaveDecision(ctx context.Context, d store.Decision) (int64, error)
	Freeze(ctx context.Context, provider, repo, ref, reason string, decisionID int64) (bool, error)
	RepoState(ctx context.Context, provider, repo string) (store.RepoState, error)
	CompletePending(ctx context.Context, decisionID int64, action string) error
}

// Reporter returns the (cached) license report of a release.
type Reporter interface {
	Report(ctx context.Context, providerName string, repo provider.Repo, ref string) (*license.Report, error)
}

// Archiver backs up the last commit before a change.
type Archiver interface {
	FindLastOpen(ctx context.Context, providerName string, upstream provider.Repo, head string, bases, paths []string) (string, error)
	Backup(ctx context.Context, req archive.Request) (archive.Result, error)
}

// Notifier queues notifications.
type Notifier interface {
	Notify(ctx context.Context, n notify.Notice) (bool, error)
}

// Target is one upstream whose default branch is watched.
type Target struct {
	Provider string
	Repo     provider.Repo
	// Kind is how its releases are watched; the newest one is the first
	// baseline.
	Kind   provider.EventKind
	Config config.Repo
}

// Watcher checks each target's default branch on an interval.
type Watcher struct {
	cfg      *config.Config
	readers  map[string]provider.BranchReader
	store    Store
	reports  Reporter
	engine   *rules.Engine
	archiver Archiver
	targets  []Target
	log      *slog.Logger
	Interval time.Duration
	// Spacing is the pause between targets.
	Spacing time.Duration
	// Notify queues notifications; nil disables them.
	Notify Notifier
}

// New returns a watcher. archiver may be nil (no backups).
func New(cfg *config.Config, readers map[string]provider.BranchReader, st Store, reports Reporter,
	engine *rules.Engine, archiver Archiver, targets []Target, log *slog.Logger) *Watcher {
	return &Watcher{cfg: cfg, readers: readers, store: st, reports: reports, engine: engine, archiver: archiver,
		targets: targets, log: log, Interval: 15 * time.Minute, Spacing: time.Second}
}

// Result is the outcome of checking one target.
type Result struct {
	Target Target
	// Skipped is non-empty when the target wasn't checked, with the reason.
	Skipped string
	// Head is the default branch head checked ("" if NotModified).
	Head        string
	NotModified bool
	// Unchanged means the top-level license files didn't change.
	Unchanged bool
	// Baseline is true when this was the first check, with no release to
	// compare with.
	Baseline bool
	// BaseRef is what the head was compared with.
	BaseRef string
	// Diff is set when the top-level license changed.
	Diff        *license.Diff
	Decision    *rules.Decision
	NewlyFrozen bool
	// LastOpen is the last commit before the change, when found.
	LastOpen  string
	RateLimit provider.RateLimit
	Err       error
}

// Run checks every target now and then every Interval until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	if len(w.targets) == 0 {
		return
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		w.CheckAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// CheckAll checks every target once.
func (w *Watcher) CheckAll(ctx context.Context) []Result {
	var out []Result
	for i, t := range w.targets {
		if ctx.Err() != nil {
			break
		}
		if i > 0 && w.Spacing > 0 {
			select {
			case <-ctx.Done():
				return out
			case <-time.After(w.Spacing):
			}
		}
		r := w.Check(ctx, t, false)
		w.logResult(r)
		out = append(out, r)
		var rl *provider.RateLimitError
		if errors.As(r.Err, &rl) {
			w.log.Warn("rate limited; skipping the rest of this round", "until", rl.Reset)
			break
		}
	}
	return out
}

// Check checks one target. With dryRun nothing is recorded or acted on.
func (w *Watcher) Check(ctx context.Context, t Target, dryRun bool) Result {
	res := Result{Target: t}
	res.Err = w.check(ctx, t, dryRun, &res)
	if res.Err != nil && !dryRun {
		if err := w.store.RecordMainWatchError(ctx, t.Provider, t.Repo.FullName(), res.Err); err != nil {
			w.log.Error("record main watch error", "err", err)
		}
	}
	return res
}

func (w *Watcher) check(ctx context.Context, t Target, dryRun bool, res *Result) error {
	repo := t.Repo.FullName()
	reader, ok := w.readers[t.Provider]
	if !ok {
		return fmt.Errorf("no branch reader for provider %q", t.Provider)
	}
	state, err := w.store.RepoState(ctx, t.Provider, repo)
	if err != nil {
		return err
	}
	if state.Frozen {
		res.Skipped = "frozen at " + orNone(state.FrozenRef)
		return nil
	}
	prev, err := w.store.MainWatch(ctx, t.Provider, repo)
	if err != nil {
		return err
	}
	etag := ""
	if prev.HeadSHA != "" && len(prev.Report) > 0 && !dryRun {
		etag = prev.ETag
	}
	head, err := reader.DefaultHead(ctx, t.Repo, etag)
	res.RateLimit = head.RateLimit
	if err != nil {
		return err
	}
	next := prev
	next.ETag = head.ETag
	if head.NotModified {
		res.NotModified, res.Unchanged = true, true
		return w.save(ctx, dryRun, next)
	}
	res.Head = head.SHA
	next.HeadSHA = head.SHA
	if head.SHA == prev.HeadSHA && len(prev.Report) > 0 {
		res.Unchanged = true
		return w.save(ctx, dryRun, next)
	}

	files, err := topLevelFiles(ctx, reader, t.Repo, head.SHA)
	if err != nil {
		return err
	}
	next.Fingerprint = fingerprint(files)
	if next.Fingerprint == prev.Fingerprint && len(prev.Report) > 0 {
		res.Unchanged = true
		return w.save(ctx, dryRun, next)
	}
	cur, err := scanFiles(ctx, reader, t.Repo, files)
	if err != nil {
		return err
	}
	curRef := "main@" + short(head.SHA)
	if next.Report, err = cur.Marshal(); err != nil {
		return err
	}
	next.BaseRef = curRef

	// Compare with the previous head; the first time, with the newest
	// release, which also catches a change that landed before Fork Keeper
	// started watching.
	base, baseRef, err := w.baseline(ctx, t, prev)
	if err != nil {
		return err
	}
	if base == nil {
		res.Baseline = true
		return w.save(ctx, dryRun, next)
	}
	res.BaseRef = baseRef
	d := license.Compare(baseRef, base, curRef, cur)
	if !d.TopLevelChanged {
		res.Unchanged = true
		return w.save(ctx, dryRun, next)
	}
	res.Diff = d
	dec := w.engine.Evaluate(repo, d)
	res.Decision = &dec
	if dryRun {
		return nil
	}
	if err := w.act(ctx, t, prev, head.SHA, base, cur, res); err != nil {
		return err
	}
	return w.save(ctx, dryRun, next)
}

func (w *Watcher) save(ctx context.Context, dryRun bool, s store.MainWatch) error {
	if dryRun {
		return nil
	}
	return w.store.SaveMainWatch(ctx, s)
}

// baseline returns the top-level report to compare the head with.
func (w *Watcher) baseline(ctx context.Context, t Target, prev store.MainWatch) (*license.Report, string, error) {
	if len(prev.Report) > 0 {
		r, err := license.UnmarshalReport(prev.Report)
		if err == nil {
			return r, prev.BaseRef, nil
		}
		w.log.Info("stored default-branch report is from an older scanner; comparing with the latest release", "repo", t.Repo.FullName())
	}
	ref, ok, err := w.store.LatestRef(ctx, t.Provider, t.Repo.FullName(), t.Kind)
	if err != nil || !ok {
		return nil, "", err
	}
	r, err := w.reports.Report(ctx, t.Provider, t.Repo, ref)
	if err != nil {
		return nil, "", fmt.Errorf("license report of %s: %w", ref, err)
	}
	return r.TopLevel(), ref, nil
}

// act records the decision, freezes if a rule says so, and backs up the
// last commit before the change (and the last release, when frozen or
// archived).
func (w *Watcher) act(ctx context.Context, t Target, prev store.MainWatch, head string, base, cur *license.Report,
	res *Result) (err error) {
	repo := t.Repo.FullName()
	d := *res.Decision
	release, _, err := w.store.LatestRef(ctx, t.Provider, repo, t.Kind)
	if err != nil {
		return err
	}
	pending := slices.DeleteFunc(slices.Clone(d.Actions), func(a string) bool {
		return a == config.ActionFreeze || a == config.ActionAllow
	})
	id, err := w.store.SaveDecision(ctx, store.Decision{
		RefKey:      store.RefKey{Provider: t.Provider, Repo: repo, Ref: res.Diff.NewRef},
		PreviousRef: res.BaseRef,
		Rule:        d.Rule,
		Scope:       d.Scope,
		Actions:     d.Actions,
		MergePolicy: string(d.MergePolicy),
		Reason:      "on the default branch, not yet released: " + d.Reason,
		Matched:     d.Matched,
		Pending:     pending,
	})
	if err != nil {
		return err
	}
	// Tell someone even if the backup below fails: the warning matters most.
	defer func() { w.notify(ctx, t, id, release, res, err) }()
	if d.Has(config.ActionFreeze) {
		if res.NewlyFrozen, err = w.store.Freeze(ctx, t.Provider, repo, release,
			"license change on the default branch: "+d.Reason, id); err != nil {
			return err
		}
	}
	// Nothing to back up when the change is accepted outright.
	acted := slices.ContainsFunc(d.Actions, func(a string) bool { return a != config.ActionAllow })
	if w.archiver == nil || !acted {
		return nil
	}

	paths := append(base.TopLevelPaths(), cur.TopLevelPaths()...)
	paths = append(paths, "LICENSES", "licenses")
	slices.Sort(paths)
	paths = slices.Compact(paths)
	lastOpen, err := w.archiver.FindLastOpen(ctx, t.Provider, t.Repo, head, []string{prev.HeadSHA, release}, paths)
	if err != nil {
		return fmt.Errorf("find the last commit before the change: %w", err)
	}
	res.LastOpen = lastOpen
	req := archive.Request{
		Provider: t.Provider,
		Upstream: t.Repo,
		Fork:     t.Config.Fork,
		Settings: w.cfg.ArchiveFor(t.Config),
		LastOpen: lastOpen,
		Reason:   fmt.Sprintf("last commit before %s on the default branch (%s)", res.Diff.NewRef, d.Rule),
		Urgent:   true,
	}
	if release != "" && (d.Has(config.ActionFreeze) || d.Has(config.ActionArchive)) {
		req.LastGood = release
	}
	if _, err := w.archiver.Backup(ctx, req); err != nil {
		return fmt.Errorf("back up %s: %w", short(lastOpen), err)
	}
	if d.Has(config.ActionArchive) {
		return w.store.CompletePending(ctx, id, config.ActionArchive)
	}
	return nil
}

// notify sends the early warning when the rule asks for it, or when the
// release will need approval.
func (w *Watcher) notify(ctx context.Context, t Target, decisionID int64, release string, res *Result, actErr error) {
	d := *res.Decision
	if w.Notify == nil || !d.Has(config.ActionNotify) && d.MergePolicy == rules.MergeAuto {
		return
	}
	repo := t.Repo.FullName()
	m := notify.Message{Kind: notify.KindLicense, Repo: repo, Ref: res.Diff.NewRef, Priority: notify.PriorityHigh,
		Tags: []string{"warning"}, Title: fmt.Sprintf("Early warning: %s is changing its license on %s", repo, res.Diff.NewRef)}
	if !d.Has(config.ActionNotify) {
		m.Kind = notify.KindApproval
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The default branch of %s changed its top-level license; no release has it yet.\n\n%s\n\n",
		repo, res.Diff.Summary())
	if d.Rule != "" {
		fmt.Fprintf(&b, "%s.\nActions: %s.\n", notify.Capitalize(d.Reason), strings.Join(d.Actions, ", "))
	}
	switch {
	case res.NewlyFrozen:
		m.Title = fmt.Sprintf("FROZEN (early warning): %s is relicensing on %s", repo, res.Diff.NewRef)
		m.Priority, m.Tags = notify.PriorityUrgent, []string{"rotating_light"}
		at := release
		if at == "" {
			at = "no version"
		}
		fmt.Fprintf(&b, "\nThe fork is frozen at %s before any release ships the change.\n", at)
	case d.MergePolicy != rules.MergeAuto:
		fmt.Fprintf(&b, "\nThe release that ships it will need your approval (%s).\n", strings.ReplaceAll(string(d.MergePolicy), "_", " "))
	}
	if res.LastOpen != "" {
		fmt.Fprintf(&b, "The last commit before the change (%s) is backed up as forkkeeper/last-open.\n", short(res.LastOpen))
	} else if actErr != nil {
		fmt.Fprintf(&b, "Backing up the last commit before the change failed and will be retried: %v\n", actErr)
	}
	m.Body = strings.TrimSpace(b.String())
	n := notify.Notice{Key: fmt.Sprintf("decision:%s/%s@%s", t.Provider, repo, res.Diff.NewRef), Provider: t.Provider, Message: m}
	if d.Has(config.ActionNotify) {
		n.DecisionID = decisionID
	}
	if _, err := w.Notify.Notify(ctx, n); err != nil {
		w.log.Warn("could not queue notification", "repo", repo, "err", err)
	}
}

// topLevelFiles lists the files that determine the top-level license at
// commit: root license files and manifests, and a LICENSES/ directory.
func topLevelFiles(ctx context.Context, reader provider.BranchReader, repo provider.Repo, commit string) ([]provider.TreeFile, error) {
	root, err := reader.ListFiles(ctx, repo, commit, "")
	if err != nil {
		return nil, err
	}
	var out []provider.TreeFile
	for _, f := range root {
		if dir, ok := strings.CutSuffix(f.Path, "/"); ok {
			if !license.IsLicensesDir(dir) {
				continue
			}
			sub, err := reader.ListFiles(ctx, repo, commit, dir)
			if err != nil {
				return nil, err
			}
			for _, s := range sub {
				if !strings.HasSuffix(s.Path, "/") && license.TopLevelPath(s.Path) {
					out = append(out, s)
				}
			}
			continue
		}
		if license.TopLevelPath(f.Path) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b provider.TreeFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func fingerprint(files []provider.TreeFile) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.Path + " " + f.ID + "\n")
	}
	return b.String()
}

// maxScanFiles bounds the files read for one check.
const maxScanFiles = 50

func scanFiles(ctx context.Context, reader provider.BranchReader, repo provider.Repo, files []provider.TreeFile) (*license.Report, error) {
	if len(files) > maxScanFiles {
		return nil, fmt.Errorf("%d top-level license files and manifests; more than the %d checked", len(files), maxScanFiles)
	}
	contents := license.Files{}
	for _, f := range files {
		b, err := reader.ReadFile(ctx, repo, f.ID)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Path, err)
		}
		contents[f.Path] = b
	}
	r, err := license.Scan(ctx, contents, license.Options{})
	if err != nil {
		return nil, err
	}
	return r.TopLevel(), nil
}

func (w *Watcher) logResult(r Result) {
	log := w.log.With("repo", r.Target.Repo.FullName())
	switch {
	case r.Err != nil:
		log.Warn("default-branch check failed", "err", r.Err)
	case r.Skipped != "":
		log.Debug("default-branch check skipped", "reason", r.Skipped)
	case r.Baseline:
		log.Info("default-branch license recorded; no release seen yet to compare with", "head", short(r.Head))
	case r.Diff != nil:
		attrs := []any{"head", short(r.Head), "compared_with", r.BaseRef, "change", r.Diff.Summary()}
		if r.Decision != nil {
			attrs = append(attrs, "rule", r.Decision.Rule, "actions", r.Decision.Actions, "merge_policy", r.Decision.MergePolicy)
		}
		if r.LastOpen != "" {
			attrs = append(attrs, "last_open", short(r.LastOpen))
		}
		if r.NewlyFrozen {
			attrs = append(attrs, "frozen", true)
		}
		log.Warn("LICENSE CHANGE ON DEFAULT BRANCH, not yet released", attrs...)
	default:
		log.Debug("default branch: top-level license unchanged", "head", short(r.Head), "not_modified", r.NotModified)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orNone(s string) string {
	if s == "" {
		return "(no version)"
	}
	return s
}
