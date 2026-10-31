package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/approval"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

const approvalsUsage = `usage:
  forkkeeper approvals [--all] [owner/repo]   list approvals (pending only by default)
  forkkeeper approvals <id>                   show one approval
  forkkeeper approve [--reason "why"] <id>    approve (a reason is required to override block_auto_merge)
  forkkeeper reject --reason "why" <id>       reject: freeze the fork and close the sync PR`

// cliActor names the person running the CLI, for the audit log.
func cliActor() string {
	// forkkeeperctl runs commands as the service user; it passes on who
	// actually ran them.
	if a := os.Getenv("FORKKEEPER_ACTOR"); a != "" {
		return "cli:" + a
	}
	if u, err := user.Current(); err == nil {
		return "cli:" + u.Username
	}
	return "cli"
}

// openDB loads the config and opens the database.
func openDB(ctx context.Context) (*config.Config, *store.Store, bool) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return nil, nil, false
	}
	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, false
	}
	return cfg, db, true
}

func approvalsCmd(args []string) int {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, approvalsUsage) }
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	_, db, ok := openDB(ctx)
	if !ok {
		return 1
	}
	defer db.Close()

	if id, err := strconv.ParseInt(fs.Arg(0), 10, 64); err == nil {
		return showApproval(ctx, db, id)
	}
	status := store.ApprovalPending
	if *all {
		status = ""
	}
	list, err := db.Approvals(ctx, fs.Arg(0), status, 50)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(list) == 0 {
		if *all {
			fmt.Println("No approvals yet.")
		} else {
			fmt.Println("Nothing is waiting for approval.")
		}
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREPO\tVERSION\tNEEDS\tRULE\tSTATUS\tSINCE\tPR")
	for _, a := range list {
		var dc notify.DecisionContext
		_ = json.Unmarshal(a.Context, &dc)
		version := a.Ref
		if dc.PreviousRef != "" {
			version = dc.PreviousRef + " -> " + a.Ref
		}
		status := a.Status
		if a.Status != store.ApprovalPending {
			status += " by " + a.ResolvedBy
		}
		pr := "-"
		if s, ok, _ := db.SyncFor(ctx, a.RefKey); ok && s.PRURL != "" {
			pr = s.PRURL
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Repo, version, a.Kind, orDash(dc.Rule), status,
			a.CreatedAt.Local().Format(time.DateTime), pr)
	}
	tw.Flush()
	if !*all {
		fmt.Println("\nforkkeeper approvals <id> shows one; approve <id> or reject --reason \"why\" <id> resolves it.")
	}
	return 0
}

func showApproval(ctx context.Context, db *store.Store, id int64) int {
	a, ok, err := db.Approval(ctx, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "no approval #%d\n", id)
		return 1
	}
	var dc notify.DecisionContext
	_ = json.Unmarshal(a.Context, &dc)
	fmt.Printf("Approval #%d: %s %s\n", a.ID, a.Repo, a.Ref)
	fmt.Printf("Needs:        %s\n", map[string]string{store.ApprovalNeeded: "approval (require_approval)",
		store.ApprovalOverride: "an override (block_auto_merge)"}[a.Kind])
	fmt.Printf("Previous:     %s\n", orDash(dc.PreviousRef))
	fmt.Printf("Rule:         %s\n", orDash(dc.Rule))
	fmt.Printf("Reason:       %s\n", orDash(dc.Reason))
	fmt.Printf("Actions:      %s\n", orDash(strings.Join(dc.Actions, ", ")))
	fmt.Printf("Created:      %s\n", a.CreatedAt.Local().Format(time.DateTime))
	if a.Status == store.ApprovalPending {
		fmt.Printf("Status:       pending")
		if a.Reminders > 0 {
			fmt.Printf(" (%d reminder(s) sent)", a.Reminders)
		}
		fmt.Println()
	} else {
		fmt.Printf("Status:       %s by %s on %s\n", a.Status, a.ResolvedBy, a.ResolvedAt.Local().Format(time.DateTime))
		if a.Resolution != "" {
			fmt.Printf("Note:         %s\n", a.Resolution)
		}
	}
	if s, ok, _ := db.SyncFor(ctx, a.RefKey); ok {
		fmt.Printf("Sync:         %s on %s", s.Status, s.Fork)
		if s.PRURL != "" {
			fmt.Printf(", %s", s.PRURL)
		}
		fmt.Println()
	}
	if checks, err := db.LicenseChecks(ctx, a.Repo, 50); err == nil {
		for _, c := range checks {
			if c.Ref == a.Ref && c.Summary != "" {
				fmt.Printf("\n%s\n", c.Summary)
				break
			}
		}
	}
	return 0
}

// approveCmd approves or rejects one approval.
func approveCmd(args []string, approve bool) int {
	name := map[bool]string{true: "approve", false: "reject"}[approve]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, approvalsUsage) }
	reason := fs.String("reason", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(fs.Arg(0), "#"), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%q is not an approval ID (see forkkeeper approvals)\n", fs.Arg(0))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cfg, db, ok := openDB(ctx)
	if !ok {
		return 1
	}
	defer db.Close()

	auth := newGitHubAuth(cfg, newCredentials(cfg.Credentials))
	hosts := map[string]provider.ForkHost{github.Name: github.NewForks(cfg.GitHub.APIURL, repoToken(cfg, auth), nil)}
	svc := approval.New(db, hosts, newLogger().With("component", "approval"))
	svc.AutoMerge = cfg.Merge.AutoMergeEnabled()

	var res approval.Result
	if approve {
		res, err = svc.Approve(ctx, id, cliActor(), *reason)
	} else {
		res, err = svc.Reject(ctx, id, cliActor(), *reason)
	}
	a := res.Approval
	if a.ID == 0 {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("Approval #%d (%s %s): %s by %s.\n", a.ID, a.Repo, a.Ref, a.Status, a.ResolvedBy)
	if d := notify.Capitalize(res.Detail); d != "" {
		if !strings.HasSuffix(d, ".") {
			d += "."
		}
		fmt.Println(d)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "The decision is recorded, but updating the fork failed: %v\n", err)
		if approve {
			fmt.Fprintln(os.Stderr, "The service retries it on its next PR check.")
		}
		return 1
	}
	return 0
}

const freezeUsage = `usage: forkkeeper freeze --reason "why" [--at <ref>] <owner/repo>

Stops syncing the fork: no new upstream version is merged, and open sync PRs
are not auto-merged, until forkkeeper unfreeze. --at records the last good
version (default: the newest release seen).`

func freezeCmd(args []string) int {
	fs := flag.NewFlagSet("freeze", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, freezeUsage) }
	reason := fs.String("reason", "", "")
	at := fs.String("at", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || strings.TrimSpace(*reason) == "" {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	cfg, db, ok := openDB(ctx)
	if !ok {
		return 1
	}
	defer db.Close()
	repoCfg, ok := cfg.RepoFor(fs.Arg(0))
	if !ok {
		fmt.Fprintf(os.Stderr, "%s is not a configured upstream\n", fs.Arg(0))
		return 1
	}
	repo := repoCfg.Upstream
	ref := *at
	if ref == "" {
		latest, _, err := db.LatestRef(ctx, github.Name, repo, watchKind(repoCfg))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		ref = latest
	}
	frozen, err := db.FreezeBy(ctx, github.Name, repo, ref, cliActor(), *reason, 0, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !frozen {
		st, _ := db.RepoState(ctx, github.Name, repo)
		fmt.Fprintf(os.Stderr, "%s is already frozen at %s (since %s)\n", repo, orDash(st.FrozenRef),
			st.FrozenAt.Local().Format(time.DateTime))
		return 1
	}
	fmt.Printf("%s frozen at %s.\n", repo, orDash(ref))
	return 0
}

func auditCmd(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "number of entries")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: forkkeeper audit [--limit n] [owner/repo]") }
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	_, db, ok := openDB(ctx)
	if !ok {
		return 1
	}
	defer db.Close()
	entries, err := db.AuditLog(ctx, fs.Arg(0), *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Println("The audit log is empty.")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tREPO\tACTION\tBY\tDETAIL\tREASON")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.CreatedAt.Local().Format(time.DateTime), e.Repo, e.Kind, e.Actor,
			e.Detail, orDash(e.Reason))
	}
	tw.Flush()
	return 0
}

const notifyUsage = `usage:
  forkkeeper notify test   send a test message to every configured channel
  forkkeeper notify list   show recent notifications and whether they were sent`

func notifyCmd(args []string) int {
	if len(args) != 1 || (args[0] != "test" && args[0] != "list") {
		fmt.Fprintln(os.Stderr, notifyUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if args[0] == "list" {
		_, db, ok := openDB(ctx)
		if !ok {
			return 1
		}
		defer db.Close()
		list, err := db.Notifications(ctx, 30)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(list) == 0 {
			fmt.Println("No notifications yet.")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "QUEUED\tKIND\tTITLE\tSTATUS\tCHANNELS\tERROR")
		for _, n := range list {
			var m notify.Message
			_ = json.Unmarshal(n.Message, &m)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n.CreatedAt.Local().Format(time.DateTime), n.Kind, m.Title, n.Status,
				orDash(strings.Join(n.Sent, ", ")), orDash(n.LastError))
		}
		tw.Flush()
		return 0
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	channels := notify.Channels(cfg.Notifications, newCredentials(cfg.Credentials), nil)
	if len(channels) == 0 {
		fmt.Fprintln(os.Stderr, "no notification channels configured (see notifications: in the example config)")
		return 1
	}
	m := notify.Message{Kind: notify.KindTest, Title: "Fork Keeper test notification", Priority: notify.PriorityDefault,
		Tags: []string{"white_check_mark"},
		Body: "If you can read this, Fork Keeper can reach you here. License warnings, approvals and sync problems " +
			"will arrive the same way."}
	failed := 0
	for name, err := range notify.Send(ctx, channels, m) {
		if err != nil {
			failed++
			fmt.Printf("%-8s FAILED: %v\n", name, err)
		} else {
			fmt.Printf("%-8s sent\n", name)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}
