package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/watcher"
)

// pollCmd polls the configured upstreams once and prints what it found.
//
//	forkkeeper poll [--dry-run] [--repo owner/name]
//
// --dry-run only reads from GitHub: it doesn't record releases or queue
// anything, so it's safe for checking access while the service runs.
func pollCmd(args []string) int {
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "list releases without recording or queuing anything")
	only := fs.String("repo", "", "poll only this upstream (owner/name)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	log := newLogger()
	cfg, err := loadConfig()
	if err != nil {
		log.Error("invalid config", "err", err)
		return 1
	}
	if *only != "" {
		var kept []config.Repo
		for _, r := range cfg.Repos {
			if strings.EqualFold(r.Upstream, *only) {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			fmt.Fprintf(os.Stderr, "%s is not in the repos list of the config\n", *only)
			return 1
		}
		cfg.Repos = kept
	}
	if len(cfg.Repos) == 0 {
		fmt.Fprintln(os.Stderr, "no repos configured; add upstreams under repos: in the config")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	auth := newGitHubAuth(cfg, newCredentials(cfg.Credentials))

	var st watcher.Store
	if !*dryRun {
		db, err := store.Open(ctx, cfg.Database.Path)
		if err != nil {
			log.Error("open database", "err", err)
			return 1
		}
		defer db.Close()
		st = db
	}
	poller := newPoller(cfg, auth, st, log)

	var results []watcher.Result
	for _, t := range pollTargets(cfg) {
		results = append(results, poller.PollOne(ctx, t, *dryRun))
	}
	printResults(os.Stdout, cfg, results, *dryRun)

	for _, r := range results {
		if r.Err != nil {
			return 1
		}
	}
	return 0
}

func printResults(w io.Writer, cfg *config.Config, results []watcher.Result, dryRun bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UPSTREAM\tWATCH\tAUTH\tRESULT\tLATEST\tPUBLISHED\tLATEST PRE-RELEASE")
	var quota provider.RateLimit
	for _, r := range results {
		auth := cfg.PollAuthProfile(r.Target.Repo.FullName())
		if auth == "" {
			auth = config.Anonymous
		}
		stable, pre := latestRefs(r.Refs)
		published := "-"
		if !stable.PublishedAt.IsZero() {
			published = stable.PublishedAt.Format(time.DateOnly)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Target.Repo.FullName(), watchName(r.Target.Kind),
			auth, resultText(r, dryRun), orDash(stable.Name), published, orDash(pre.Name))
		if r.RateLimit.Limit > 0 {
			quota = r.RateLimit
		}
	}
	tw.Flush()
	if quota.Limit > 0 {
		fmt.Fprintf(w, "\nGitHub API quota: %d of %d requests left, resets %s\n",
			quota.Remaining, quota.Limit, quota.Reset.Local().Format(time.Kitchen))
	}
	if dryRun {
		fmt.Fprintln(w, "Dry run: nothing was recorded or queued.")
	}
}

func resultText(r watcher.Result, dryRun bool) string {
	switch {
	case r.Err != nil:
		return "ERROR: " + r.Err.Error()
	case r.NotModified:
		return "no change"
	case dryRun:
		return fmt.Sprintf("ok, %d found", len(r.Refs))
	case r.Outcome.Baseline:
		return fmt.Sprintf("baseline, %d recorded", len(r.Outcome.New))
	case len(r.Outcome.Queued) > 0:
		tags := make([]string, len(r.Outcome.Queued))
		for i, ev := range r.Outcome.Queued {
			tags[i] = ev.Tag
		}
		return "queued " + strings.Join(tags, ", ")
	case len(r.Outcome.New) > 0:
		return fmt.Sprintf("%d new pre-release(s), not queued", len(r.Outcome.New))
	default:
		return "no new releases"
	}
}

// latestRefs returns the newest stable and pre-release refs. Releases come
// newest first; tag lists have no date order, so only the first is shown.
func latestRefs(refs []provider.Ref) (stable, pre provider.Ref) {
	for _, ref := range refs {
		if ref.Prerelease {
			if pre.Name == "" {
				pre = ref
			}
		} else if stable.Name == "" {
			stable = ref
		}
	}
	return stable, pre
}

func watchName(k provider.EventKind) string {
	if k == provider.KindTag {
		return config.WatchTags
	}
	return config.WatchReleases
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
