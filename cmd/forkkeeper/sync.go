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
	"time"

	"github.com/mightyheretic/fork-keeper/internal/gate"
	"github.com/mightyheretic/fork-keeper/internal/merge"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/pipeline"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

const syncUsage = `usage: forkkeeper sync [--dry-run] [--force] <owner/upstream> <ref>

Runs the license check and rules for <ref>, then merges it into the
configured fork on the staging branch and opens or updates the PR.

  --dry-run  compute the merge locally; push nothing, open no PR, record
             no decision
  --force    sync even if <ref> was already synced or is older than a
             synced version`

// syncCmd processes one upstream version by hand, like the service would.
func syncCmd(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, syncUsage) }
	dryRun := fs.Bool("dry-run", false, "")
	force := fs.Bool("force", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	upstream, err := provider.ParseRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ref := fs.Arg(1)
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	repoCfg, ok := cfg.RepoFor(upstream.FullName())
	if !ok || repoCfg.Fork == "" {
		fmt.Fprintf(os.Stderr, "%s has no fork configured (set repos[].fork)\n", upstream.FullName())
		return 1
	}
	fork, _ := provider.ParseRepo(repoCfg.Fork)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
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
	engine, _, err := newMergeEngine(cfg, auth, db, log.With("component", "merge"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	licenseGate := gate.New(db, map[string]provider.ArchiveFetcher{
		github.Name: github.NewArchives(cfg.GitHub.APIURL, upstreamToken(cfg, auth)),
	}, log.With("component", "gate"))
	ev := provider.Event{Provider: github.Name, Kind: provider.KindRelease, Repo: upstream, Tag: ref,
		Source: "cli", ReceivedAt: time.Now()}

	if !*dryRun {
		pipe := pipeline.New(cfg, licenseGate, rules.New(cfg), db, engine, log.With("component", "pipeline"))
		pipe.ForceSync = *force
		pipe.Notify = notify.NewQueue(db, cfg.Notifications)
		if archiver, err := newArchiver(cfg, auth, creds, db, log.With("component", "archive")); err == nil {
			pipe.Archiver = archiver
		}
		out, err := pipe.Run(ctx, ev)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("Merge policy: %s (%s)\n", out.Decision.MergePolicy, orDash(out.Decision.Reason))
		if a := out.Approval; a != nil && a.Status == store.ApprovalPending {
			fmt.Printf("Needs approval #%d: forkkeeper approve %d (or reject --reason \"why\" %d)\n", a.ID, a.ID, a.ID)
		}
		for _, a := range out.Waiting {
			fmt.Printf("Held: %s is still waiting for approval #%d\n", a.Ref, a.ID)
		}
		if out.Sync == nil {
			fmt.Printf("Not synced: %s is frozen at %s.\n", upstream.FullName(), orDash(out.State.FrozenRef))
			return 1
		}
		writeSync(os.Stdout, *out.Sync)
		return syncExit(*out.Sync)
	}

	res, err := licenseGate.Check(ctx, ev)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var d rules.Decision
	if res.Diff == nil {
		d = rules.New(cfg).EvaluateFirst(upstream.FullName(), res.Report)
	} else {
		d = rules.New(cfg).Evaluate(upstream.FullName(), res.Diff)
	}
	fmt.Printf("Merge policy: %s (%s)\n", d.MergePolicy, orDash(d.Reason))
	state, err := db.RepoState(ctx, github.Name, upstream.FullName())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if d.MergePolicy == rules.MergeFrozen || state.Frozen {
		fmt.Println("Dry run: would not sync, the repo would be (or is) frozen.")
		return 0
	}
	req := pipeline.SyncRequest(ev, fork, repoCfg, res, d)
	req.DryRun, req.Force = true, *force
	out, err := engine.Sync(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("Dry run: nothing was pushed and no PR was opened.")
	writeSync(os.Stdout, out)
	return syncExit(out)
}

// syncExit is 0 for success, 3 for conflicts (like license diff's 3 for a
// change), so scripts can tell them apart from errors.
func syncExit(r merge.Result) int {
	if r.Status == store.SyncConflict {
		return 3
	}
	return 0
}

func writeSync(w io.Writer, r merge.Result) {
	if r.Already {
		fmt.Fprintf(w, "Already synced (%s).\n", r.Status)
	}
	fmt.Fprintf(w, "Status:       %s\n", r.Status)
	if r.Detail != "" {
		fmt.Fprintf(w, "Detail:       %s\n", r.Detail)
	}
	if r.Strategy != "" {
		fmt.Fprintf(w, "Strategy:     %s\n", r.Strategy)
	}
	if r.StableBranch != "" {
		fmt.Fprintf(w, "Branches:     %s <- %s\n", r.StableBranch, r.StagingBranch)
	}
	if r.HeadSHA != "" {
		fmt.Fprintf(w, "Staging head: %s\n", r.HeadSHA)
	}
	if r.PatchBranch != "" {
		fmt.Fprintf(w, "Patch layer:  %s\n", r.PatchBranch)
	}
	if r.PRURL != "" {
		fmt.Fprintf(w, "Pull request: %s\n", r.PRURL)
	}
	if len(r.Patches) > 0 {
		dropped := map[string]bool{}
		for _, p := range r.Dropped {
			dropped[p.SHA] = true
		}
		fmt.Fprintf(w, "Local patches (%d):\n", len(r.Patches))
		for _, p := range r.Patches {
			note := ""
			if dropped[p.SHA] {
				note = "  [dropped: already upstream]"
			}
			fmt.Fprintf(w, "  %.10s %s%s\n", p.SHA, p.Subject, note)
		}
	}
	if len(r.Conflicts) > 0 {
		fmt.Fprintf(w, "Conflicts (%d):\n  %s\n", len(r.Conflicts), strings.Join(r.Conflicts, "\n  "))
	}
}
