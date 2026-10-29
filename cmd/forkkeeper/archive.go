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

	"github.com/mightyheretic/fork-keeper/internal/archive"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/gate"
	"github.com/mightyheretic/fork-keeper/internal/mainwatch"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

const archiveUsage = `usage:
  forkkeeper archive [--releases policy] [--dry-run] <owner/upstream> [tag...]
  forkkeeper archive list [owner/upstream]

Backs up the source (git history and tags, no release assets) of an
upstream's releases into <data_dir>/archive, then pushes the backups to the
fork and mirrors and asks Software Heritage to archive the upstream.

With tags, backs up those tags. Without, backs up every upstream tag the
archive policy wants (archive.releases, or --releases major|minor|all):
use it once to back up a repo's past releases.

  --dry-run  list the tags that would be backed up; fetch nothing

"list" shows what is backed up and where it was pushed.`

// archiveCmd backs up releases by hand, or lists backups.
func archiveCmd(args []string) int {
	if len(args) > 0 && args[0] == "list" {
		return archiveListCmd(args[1:])
	}
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, archiveUsage) }
	policy := fs.String("releases", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	upstream, err := provider.ParseRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	repoCfg, ok := cfg.RepoFor(upstream.FullName())
	if !ok {
		fmt.Fprintf(os.Stderr, "note: %s is not in repos:, so top-level archive settings apply and nothing is pushed to a fork\n", upstream.FullName())
		repoCfg = config.Repo{Upstream: upstream.FullName()}
	}
	settings := cfg.ArchiveFor(repoCfg)
	if *policy != "" {
		switch *policy {
		case config.ReleasesMajor, config.ReleasesMinor, config.ReleasesAll:
			settings.Releases = *policy
		default:
			fmt.Fprintln(os.Stderr, "--releases: want major, minor or all")
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	log := newLogger()
	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	creds := newCredentials(cfg.Credentials)
	auth := newGitHubAuth(cfg, creds)
	archiver, err := newArchiver(cfg, auth, creds, db, log.With("component", "archive"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	tags := fs.Args()[1:]
	if len(tags) == 0 {
		all, err := archiver.RemoteTags(ctx, github.Name, upstream)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, t := range all {
			if archive.Wants(settings.Releases, t) {
				tags = append(tags, t)
			}
		}
		fmt.Printf("%d of %d upstream tags match the %q policy.\n", len(tags), len(all), settings.Releases)
		if len(tags) == 0 {
			return 0
		}
	}
	if *dryRun {
		for _, t := range tags {
			fmt.Println("  " + t)
		}
		return 0
	}
	fmt.Printf("Backing up %d tag(s) of %s; the first backup of a large repo takes a while...\n", len(tags), upstream.FullName())
	res, err := archiver.Backup(ctx, archive.Request{
		Provider: github.Name, Upstream: upstream, Fork: repoCfg.Fork, Settings: settings, Tags: tags,
		Reason: "backed up by hand",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	writeBackup(os.Stdout, res, archiver.Dir(github.Name, upstream))
	for _, p := range res.Pushes {
		if p.Err != nil {
			return 1
		}
	}
	return 0
}

func writeBackup(w io.Writer, r archive.Result, dir string) {
	fmt.Fprintf(w, "Backed up:         %s\n", listOrNone(r.Added))
	fmt.Fprintf(w, "Already backed up: %s\n", listOrNone(r.Existing))
	if len(r.Missing) > 0 {
		fmt.Fprintf(w, "Not upstream:      %s\n", strings.Join(r.Missing, ", "))
	}
	fmt.Fprintf(w, "Local archive:     %s\n", dir)
	for _, p := range r.Pushes {
		if p.Err != nil {
			fmt.Fprintf(w, "Push %s: FAILED: %v\n", p.Destination, p.Err)
		} else {
			fmt.Fprintf(w, "Push %s: %d ref(s)\n", p.Destination, p.Refs)
		}
	}
	switch {
	case r.SWH != nil:
		fmt.Fprintf(w, "Software Heritage: %s (task %s, request %d)\n", r.SWH.Status, r.SWH.TaskStatus, r.SWH.ID)
	case r.SWHNote != "":
		fmt.Fprintf(w, "Software Heritage: %s\n", r.SWHNote)
	}
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	if len(s) > 12 {
		return fmt.Sprintf("%s ... (%d in all)", strings.Join(s[:12], ", "), len(s))
	}
	return strings.Join(s, ", ")
}

func archiveListCmd(args []string) int {
	repo := ""
	if len(args) > 0 {
		repo = args[0]
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	refs, err := db.ArchivedRefs(ctx, "", repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pushes, err := db.ArchivePushes(ctx, repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(refs) == 0 {
		fmt.Println("Nothing backed up yet.")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tREF\tKIND\tCOMMIT\tBACKED UP\tREASON")
	for _, r := range refs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Repo, r.Name, r.Kind, shortSHA(r.CommitSHA),
			r.UpdatedAt.Local().Format("2006-01-02 15:04"), r.Reason)
	}
	tw.Flush()
	if len(pushes) > 0 {
		fmt.Println()
		tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "REPO\tDESTINATION\tLAST PUSH\tREFS\tRESULT")
		for _, p := range pushes {
			result := "ok"
			if !p.OK {
				result = "failed: " + p.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", p.Repo, p.Destination, p.PushedAt.Local().Format("2006-01-02 15:04"), p.Refs, result)
		}
		tw.Flush()
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if seen[r.Repo] {
			continue
		}
		seen[r.Repo] = true
		if s, ok, err := db.LastSWHRequest(ctx, r.Provider, r.Repo); err == nil && ok {
			fmt.Printf("\nSoftware Heritage, %s: %s %s on %s", r.Repo, s.Status, s.TaskStatus, s.RequestedAt.Local().Format("2006-01-02 15:04"))
			if s.Status == "error" {
				fmt.Printf(" (%s)", s.Reason)
			}
			fmt.Println()
		}
	}
	return 0
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

const mainUsage = `usage: forkkeeper main [--dry-run] [owner/upstream]

Runs the default-branch early warning once: checks whether the top-level
license changed on each upstream's default branch since the last check (or
since the latest release, the first time).

  --dry-run  check and print, but record nothing and take no action`

// mainCmd runs the default-branch early warning once.
func mainCmd(args []string) int {
	fs := flag.NewFlagSet("main", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, mainUsage) }
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	targets := mainTargets(cfg)
	if fs.NArg() == 1 {
		var kept []mainwatch.Target
		for _, t := range targets {
			if strings.EqualFold(t.Repo.FullName(), fs.Arg(0)) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			fmt.Fprintf(os.Stderr, "%s is not a configured upstream with the early warning on\n", fs.Arg(0))
			return 1
		}
		targets = kept
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log := newLogger()
	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	creds := newCredentials(cfg.Credentials)
	auth := newGitHubAuth(cfg, creds)
	licenseGate := gate.New(db, map[string]provider.ArchiveFetcher{
		github.Name: github.NewArchives(cfg.GitHub.APIURL, upstreamToken(cfg, auth)),
	}, log.With("component", "gate"))
	archiver, err := newArchiver(cfg, auth, creds, db, log.With("component", "archive"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	w := newMainWatcher(cfg, auth, db, licenseGate, archiver, log.With("component", "mainwatch"))

	code := 0
	for _, t := range targets {
		r := w.Check(ctx, t, *dryRun)
		name := t.Repo.FullName()
		switch {
		case r.Err != nil:
			fmt.Printf("%s: error: %v\n", name, r.Err)
			code = 1
		case r.Skipped != "":
			fmt.Printf("%s: skipped (%s)\n", name, r.Skipped)
		case r.Baseline:
			fmt.Printf("%s: recorded the default branch's license at %s; no release seen yet to compare with\n", name, shortSHA(r.Head))
		case r.Diff != nil:
			fmt.Printf("%s: LICENSE CHANGE on the default branch at %s, not yet released\n  %s\n", name, shortSHA(r.Head), r.Diff.Summary())
			if r.Decision != nil {
				fmt.Printf("  rule %s: %s (merge policy %s)\n", orDash(r.Decision.Rule), strings.Join(r.Decision.Actions, ", "), r.Decision.MergePolicy)
			}
			if r.LastOpen != "" {
				fmt.Printf("  last commit before the change: %s (backed up as %s)\n", r.LastOpen, archive.LastOpenName)
			}
			if r.NewlyFrozen {
				fmt.Println("  repo frozen")
			}
			if code == 0 {
				code = 3
			}
		case r.NotModified:
			fmt.Printf("%s: default branch unchanged since the last check\n", name)
		default:
			base := r.BaseRef
			if base == "" {
				base = "the last check"
			}
			fmt.Printf("%s: top-level license at %s unchanged from %s\n", name, shortSHA(r.Head), base)
		}
	}
	return code
}
