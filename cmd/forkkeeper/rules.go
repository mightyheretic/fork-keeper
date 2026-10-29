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

	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

const rulesUsage = `usage:
  forkkeeper rules list [owner/repo]
  forkkeeper rules test <owner/repo> <old-ref> <new-ref>

list shows the license rules (for one repo: the ones that apply to it), in
the order they win. Rules live in the config file (license.rules), so they
are reviewed and versioned like the rest of it; edit it and restart.

test downloads both versions, diffs their licenses and shows which rule
would fire and what it would do. Nothing is recorded or frozen.`

// rulesCmd lists the rules, or dry-runs the rule engine against two real
// versions.
func rulesCmd(args []string) int {
	if len(args) >= 1 && len(args) <= 2 && args[0] == "list" {
		return rulesList(args[1:])
	}
	if len(args) != 4 || args[0] != "test" {
		fmt.Fprintln(os.Stderr, rulesUsage)
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	if _, err := provider.ParseRepo(args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	sc := &cliScanner{archives: github.NewArchives(cfg.GitHub.APIURL,
		upstreamToken(cfg, newGitHubAuth(cfg, newCredentials(cfg.Credentials))))}
	oldR, err := sc.scan(ctx, args[1]+"@"+args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	newR, err := sc.scan(ctx, args[1]+"@"+args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	diff := license.Compare(args[2], oldR, args[3], newR)
	if _, ok := cfg.RepoFor(args[1]); !ok {
		fmt.Fprintf(os.Stderr, "note: %s is not in repos:, so only global rules apply\n", args[1])
	}
	writeDecision(os.Stdout, diff, rules.New(cfg).Evaluate(args[1], diff))
	return 0
}

func rulesList(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	repo := ""
	if len(args) == 1 {
		repo = args[0]
		if _, ok := cfg.RepoFor(repo); !ok {
			fmt.Fprintf(os.Stderr, "note: %s is not in repos:, so only global rules apply\n", repo)
		}
	}
	e := rules.New(cfg)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tRULE\tSCOPE\tFROM\tTO\tREMOVED\tACTIONS")
	list := func(v []string) string { return orDash(strings.Join(v, ", ")) }
	for i, r := range e.RulesFor(repo) {
		scope := r.Scope
		switch {
		case len(r.Repos) > 0:
			scope += " (" + strings.Join(r.Repos, ", ") + ")"
		case len(r.Groups) > 0:
			scope += " (" + strings.Join(r.Groups, ", ") + ")"
		}
		if r.Priority != 0 {
			scope += fmt.Sprintf(", priority %d", r.Priority)
		}
		from := r.From
		if len(from) == 0 {
			from = []string{"any"}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, r.Name, scope, list(from), list(r.To), list(r.Removed),
			list(r.Actions))
	}
	tw.Flush()
	defaults, escalation := e.Defaults()
	fmt.Printf("\nNo rule matched:          %s\n", list(defaults))
	fmt.Printf("Sub-license escalation:  %s\n", list(escalation))
	fmt.Printf("Allowlist:               %s\n", list(cfg.License.Allowlist))
	if repo != "" {
		if r, ok := cfg.RepoFor(repo); ok {
			fmt.Printf("Sub-licenses (%s):  %s\n", repo, r.SubLicenses)
		}
	}
	return 0
}

func writeDecision(w io.Writer, diff *license.Diff, d rules.Decision) {
	fmt.Fprintln(w, diff.Summary())
	fmt.Fprintln(w)
	if d.Rule == "" {
		fmt.Fprintf(w, "Decision: no action (%s); merge policy %s\n", d.Reason, d.MergePolicy)
		return
	}
	fmt.Fprintf(w, "Rule:         %s (%s scope)\n", d.Rule, d.Scope)
	fmt.Fprintf(w, "Actions:      %s\n", strings.Join(d.Actions, ", "))
	fmt.Fprintf(w, "Merge policy: %s\n", d.MergePolicy)
	fmt.Fprintf(w, "Reason:       %s\n", d.Reason)
	if len(d.Matched) > 1 {
		fmt.Fprintf(w, "Also matched: %s (lower precedence)\n", strings.Join(d.Matched[1:], ", "))
	}
}

// statusCmd shows frozen repos, recent decisions and recent fork syncs.
//
//	forkkeeper status [owner/repo]
func statusCmd(args []string) int {
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

	frozen, err := db.FrozenRepos(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if len(frozen) == 0 {
		fmt.Fprintln(os.Stdout, "No frozen repos.")
	} else {
		fmt.Fprintln(tw, "FROZEN REPO\tAT\tSINCE\tREASON")
		for _, st := range frozen {
			if repo != "" && !strings.EqualFold(st.Repo, repo) {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", st.Repo, orDash(st.FrozenRef), st.FrozenAt.Local().Format(time.DateTime), st.FrozenReason)
		}
		tw.Flush()
	}

	decisions, err := db.Decisions(ctx, repo, 20)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stdout)
	if len(decisions) == 0 {
		fmt.Fprintln(os.Stdout, "No license decisions yet.")
	} else {
		fmt.Fprintln(tw, "WHEN\tREPO\tVERSION\tRULE\tMERGE POLICY\tPENDING ACTIONS")
		for _, d := range decisions {
			version := d.Ref
			if d.PreviousRef != "" {
				version = d.PreviousRef + " -> " + d.Ref
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.CreatedAt.Local().Format(time.DateTime), d.Repo, version,
				orDash(d.Rule), d.MergePolicy, orDash(strings.Join(d.Pending, ", ")))
		}
		tw.Flush()
	}

	syncs, err := db.Syncs(ctx, repo, 20)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stdout)
	if len(syncs) == 0 {
		fmt.Fprintln(os.Stdout, "No fork syncs yet.")
		return 0
	}
	fmt.Fprintln(tw, "UPDATED\tFORK\tVERSION\tSTATUS\tSTRATEGY\tPR\tDETAIL")
	for _, s := range syncs {
		pr := "-"
		if s.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", s.PRNumber)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.UpdatedAt.Local().Format(time.DateTime), s.Fork, s.Ref,
			s.Status, orDash(s.Strategy), pr, orDash(s.Detail))
	}
	tw.Flush()
	return 0
}

// unfreezeCmd lifts a freeze. A reason is required and recorded.
//
//	forkkeeper unfreeze --reason "upstream reverted the license" owner/repo
func unfreezeCmd(args []string) int {
	fs := flag.NewFlagSet("unfreeze", flag.ContinueOnError)
	reason := fs.String("reason", "", "why the freeze is being lifted (required, recorded)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(os.Stderr, `usage: forkkeeper unfreeze --reason "why" <owner/repo>`)
		return 2
	}
	repo := fs.Arg(0)
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

	ok, err := db.Unfreeze(ctx, github.Name, repo, cliActor(), *reason)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "%s is not frozen\n", repo)
		return 1
	}
	fmt.Printf("%s unfrozen; the next upstream release will be checked and synced normally.\n", repo)
	return 0
}
