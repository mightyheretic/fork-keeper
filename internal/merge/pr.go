package merge

import (
	"context"
	"fmt"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
)

// openPR opens a PR from the staging branch into stable, or updates the
// one already open (a newer release replaces an older one on staging).
func (e *Engine) openPR(ctx context.Context, host provider.ForkHost, req Request, res *Result) error {
	policy := req.Decision.MergePolicy
	in := provider.PullRequestInput{
		Title: fmt.Sprintf("Sync upstream %s %s", req.Upstream.FullName(), req.Ref),
		Body:  prBody(req, res, strings.TrimSuffix(host.CloneURL(req.Fork), ".git")),
		Head:  req.Repo.StagingBranch,
		Base:  res.StableBranch,
		// A draft can't be merged by accident. GitHub's REST API can't turn
		// an open PR back into a draft, so labels carry the policy too.
		Draft: policy != rules.MergeAuto,
	}
	pr, err := host.FindPullRequest(ctx, req.Fork, in.Head, in.Base)
	if err != nil {
		return fmt.Errorf("find PR on %s: %w", req.Fork.FullName(), err)
	}
	if pr == nil {
		if pr, err = host.CreatePullRequest(ctx, req.Fork, in); err != nil {
			return fmt.Errorf("open PR on %s: %w", req.Fork.FullName(), err)
		}
	} else if pr, err = host.UpdatePullRequest(ctx, req.Fork, pr.Number, in); err != nil {
		return fmt.Errorf("update PR #%d on %s: %w", pr.Number, req.Fork.FullName(), err)
	}
	res.PR, res.PRNumber, res.PRURL = pr, pr.Number, pr.URL

	labels := []string{LabelSync}
	switch policy {
	case rules.MergeApproval:
		labels = append(labels, LabelApproval)
	case rules.MergeBlocked:
		labels = append(labels, LabelBlocked)
	}
	if req.Decision.Has(config.ActionFlagPR) {
		labels = append(labels, LabelFlagged)
	}
	// Labels are advisory on top of the draft state; a missing label
	// permission shouldn't lose the PR.
	if err := host.AddLabels(ctx, req.Fork, pr.Number, labels); err != nil {
		e.log.Warn("could not label PR", "repo", req.Fork.FullName(), "pr", pr.Number, "err", err)
	}
	return nil
}

func prBody(req Request, res *Result, forkURL string) string {
	var b strings.Builder
	up := req.Upstream.FullName()
	release := "`" + req.Ref + "`"
	if req.ReleaseURL != "" {
		release = fmt.Sprintf("[`%s`](%s)", req.Ref, req.ReleaseURL)
	}
	fmt.Fprintf(&b, "Fork Keeper merged upstream **%s** %s into `%s`.\n\n", up, release, res.StableBranch)
	if req.PreviousRef != "" {
		fmt.Fprintf(&b, "Previous upstream version: `%s`.\n\n", req.PreviousRef)
	}

	fmt.Fprintln(&b, "### License check")
	if req.License != "" {
		fmt.Fprintf(&b, "%s\n\n", req.License)
	}
	d := req.Decision
	if d.Rule != "" {
		fmt.Fprintf(&b, "Rule **%s** (%s scope): %s. Actions: %s.\n\n", d.Rule, d.Scope, d.Reason, strings.Join(d.Actions, ", "))
	}
	switch d.MergePolicy {
	case rules.MergeAuto:
		fmt.Fprintln(&b, "**Merge policy: auto.** Fork Keeper merges this PR once CI passes.")
	case rules.MergeApproval:
		fmt.Fprintln(&b, "**Merge policy: approval required.** This PR is a draft and won't be merged "+
			"automatically. Review the license change, then mark it ready and merge it yourself.")
	case rules.MergeBlocked:
		fmt.Fprintln(&b, "**Merge policy: blocked.** Fork Keeper will never merge this PR. "+
			"Merge it yourself only if you accept the license change.")
	}
	b.WriteString("\n")

	fmt.Fprintln(&b, "### How it was merged")
	switch res.Strategy {
	case config.StrategyPatchStack:
		fmt.Fprintf(&b, "Your local patches were re-applied on top of upstream `%s`, one at a time, and the result "+
			"is recorded as a merge of `%s` and upstream, so history is kept on both sides. The re-applied "+
			"patches are on [`%s`](%s/tree/%s) for review.\n\n", req.Ref, res.StableBranch, res.PatchBranch, forkURL, res.PatchBranch)
	default:
		fmt.Fprintf(&b, "A regular merge of upstream `%s` into `%s`, with full history.\n\n", req.Ref, res.StableBranch)
	}

	if len(res.Patches) == 0 {
		fmt.Fprintln(&b, "No local patches: the fork had no commits of its own.")
	} else {
		fmt.Fprintf(&b, "<details><summary>Local patches carried forward (%d)</summary>\n\n", len(res.Patches)-len(res.Dropped))
		dropped := map[string]bool{}
		for _, p := range res.Dropped {
			dropped[p.SHA] = true
		}
		for _, p := range res.Patches {
			note := ""
			if dropped[p.SHA] {
				note = " — dropped, upstream already has this change"
			}
			fmt.Fprintf(&b, "- %s %s%s\n", short(p.SHA), mdEscape(p.Subject), note)
		}
		fmt.Fprintln(&b, "\n</details>")
	}
	fmt.Fprintf(&b, "\n---\n`%s` is managed by Fork Keeper and is force-pushed on every sync; don't commit to it.\n", req.Repo.StagingBranch)
	return b.String()
}

// mdEscape keeps commit subjects from turning into markup or mentions.
func mdEscape(s string) string {
	r := strings.NewReplacer("<", "&lt;", ">", "&gt;", "@", "@​", "`", "\\`", "[", "\\[", "]", "\\]")
	return r.Replace(s)
}
