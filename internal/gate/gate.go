// Package gate runs the license check for a new upstream version: it scans
// the new ref and the previous stable ref, diffs them, and records the
// result. The rule engine (next milestone) decides what to do about it.
package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// Store is the subset of the store the gate uses.
type Store interface {
	PreviousRef(ctx context.Context, key store.RefKey, kind provider.EventKind, publishedAt time.Time) (string, bool, error)
	LicenseReport(ctx context.Context, key store.RefKey) ([]byte, bool, error)
	SaveLicenseReport(ctx context.Context, key store.RefKey, report []byte) error
	SaveLicenseCheck(ctx context.Context, c store.LicenseCheck) error
}

// Gate scans and compares upstream versions.
type Gate struct {
	store    Store
	archives map[string]provider.ArchiveFetcher // by provider name
	log      *slog.Logger
	// ScanTimeout bounds downloading and scanning one ref.
	ScanTimeout time.Duration
	Options     license.Options
}

// New returns a gate.
func New(st Store, archives map[string]provider.ArchiveFetcher, log *slog.Logger) *Gate {
	return &Gate{store: st, archives: archives, log: log, ScanTimeout: 15 * time.Minute}
}

// Result is the outcome of checking one event.
type Result struct {
	Ref      string
	Previous string // "" when this is the first version seen
	Report   *license.Report
	// Diff is nil when there is no previous version to compare with.
	Diff *license.Diff
}

// Check scans ev's ref and compares it with the previous stable ref.
func (g *Gate) Check(ctx context.Context, ev provider.Event) (Result, error) {
	key := store.RefKey{Provider: ev.Provider, Repo: ev.Repo.FullName(), Ref: ev.Tag}
	res := Result{Ref: ev.Tag}

	prev, ok, err := g.store.PreviousRef(ctx, key, ev.Kind, ev.PublishedAt)
	if err != nil {
		return res, err
	}
	if res.Report, err = g.Report(ctx, ev.Provider, ev.Repo, ev.Tag); err != nil {
		return res, err
	}

	check := store.LicenseCheck{RefKey: key}
	if ok {
		res.Previous = prev
		old, err := g.Report(ctx, ev.Provider, ev.Repo, prev)
		if err != nil {
			return res, fmt.Errorf("previous version %s: %w", prev, err)
		}
		res.Diff = license.Compare(prev, old, ev.Tag, res.Report)
		check.PreviousRef = prev
		check.Changed = res.Diff.Changed
		check.Summary = res.Diff.Summary()
		if check.Diff, err = jsonOf(res.Diff); err != nil {
			return res, err
		}
	} else {
		check.Summary = fmt.Sprintf("first version seen: %s is %s", ev.Tag, res.Report.Declared)
	}
	if err := g.store.SaveLicenseCheck(ctx, check); err != nil {
		return res, err
	}
	return res, nil
}

// Report returns the license report for repo at ref, scanning it on first use.
func (g *Gate) Report(ctx context.Context, providerName string, repo provider.Repo, ref string) (*license.Report, error) {
	key := store.RefKey{Provider: providerName, Repo: repo.FullName(), Ref: ref}
	if b, ok, err := g.store.LicenseReport(ctx, key); err != nil {
		return nil, err
	} else if ok {
		r, err := license.UnmarshalReport(b)
		switch {
		case err == nil:
			return r, nil
		case errors.Is(err, license.ErrStaleReport):
			g.log.Info("cached license report is from an older scanner; rescanning", "repo", key.Repo, "ref", ref)
		default:
			g.log.Warn("cached license report unreadable; rescanning", "repo", key.Repo, "ref", ref)
		}
	}

	fetch, ok := g.archives[providerName]
	if !ok {
		return nil, fmt.Errorf("no archive fetcher for provider %q", providerName)
	}
	ctx, cancel := context.WithTimeout(ctx, g.ScanTimeout)
	defer cancel()
	start := time.Now()
	body, err := fetch.Archive(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	r, err := license.Scan(ctx, license.TarGz{R: body, StripComponents: 1}, g.Options)
	if err != nil {
		return nil, fmt.Errorf("scan %s@%s: %w", key.Repo, ref, err)
	}
	g.log.Info("license scan complete", "repo", key.Repo, "ref", ref, "declared", r.Declared,
		"license_files", len(r.Files), "files_scanned", r.Headers.Scanned, "took", time.Since(start).Round(time.Millisecond).String())

	b, err := r.Marshal()
	if err != nil {
		return nil, err
	}
	if err := g.store.SaveLicenseReport(ctx, key, b); err != nil {
		return nil, err
	}
	return r, nil
}

func jsonOf(d *license.Diff) ([]byte, error) {
	b, err := d.Marshal()
	if err != nil {
		return nil, fmt.Errorf("encode diff: %w", err)
	}
	return b, nil
}
