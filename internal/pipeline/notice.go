package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// notify queues a message about the outcome: the rule's notify action, a
// version that needs approval, a merge conflict, or (when no other message
// already links it) the sync PR that was opened or updated. Failing to queue is
// logged, not returned: the work itself is done and must not be retried
// for a message.
func (p *Pipeline) notify(ctx context.Context, log *slog.Logger, ev provider.Event, decisionID int64, out *Outcome) {
	if p.Notify == nil {
		return
	}
	d := out.Decision
	needsApproval := out.Approval != nil && out.Approval.Status == store.ApprovalPending
	var notices []notify.Notice
	if d.Has(config.ActionNotify) || needsApproval {
		n := notify.Notice{Key: fmt.Sprintf("decision:%s/%s@%s", ev.Provider, ev.Repo.FullName(), ev.Tag),
			Provider: ev.Provider, Message: DecisionMessage(ev, out)}
		if d.Has(config.ActionNotify) {
			n.DecisionID = decisionID
		}
		notices = append(notices, n)
	}
	if s := out.Sync; s != nil && len(notices) == 0 && !s.Already && s.PR != nil {
		notices = append(notices, notify.Notice{Key: fmt.Sprintf("pr:%s/%s#%d@%s", ev.Provider, s.Fork, s.PRNumber, ev.Tag),
			Provider: ev.Provider, Message: prMessage(ev, out)})
	}
	if out.Sync != nil && out.Sync.Status == store.SyncConflict {
		notices = append(notices, notify.Notice{Key: fmt.Sprintf("conflict:%s/%s@%s", ev.Provider, ev.Repo.FullName(), ev.Tag),
			Provider: ev.Provider, Message: conflictMessage(ev, out)})
	}
	for _, n := range notices {
		if _, err := p.Notify.Notify(ctx, n); err != nil {
			log.Warn("could not queue notification", "kind", n.Message.Kind, "err", err)
		}
	}
}

// DecisionMessage describes a license decision on a release.
func DecisionMessage(ev provider.Event, out *Outcome) notify.Message {
	d := out.Decision
	repo := ev.Repo.FullName()
	m := notify.Message{Kind: notify.KindLicense, Repo: repo, Ref: ev.Tag, Priority: notify.PriorityDefault,
		Tags: []string{"scroll"}, Click: ev.URL}
	if !d.Has(config.ActionNotify) {
		m.Kind = notify.KindApproval
	}
	var b strings.Builder
	switch {
	case out.Check.Diff != nil:
		b.WriteString(out.Check.Diff.Summary())
	case out.Check.Report != nil:
		fmt.Fprintf(&b, "First version checked: %s %s is %s.", repo, ev.Tag, out.Check.Report.Declared)
	}
	b.WriteString("\n\n")
	if d.Rule != "" {
		fmt.Fprintf(&b, "%s.\nActions: %s.\n", notify.Capitalize(d.Reason), strings.Join(d.Actions, ", "))
	}

	switch {
	case out.NewlyFrozen:
		at := out.State.FrozenRef
		if at == "" {
			at = "no version (none was synced yet)"
		}
		m.Title = fmt.Sprintf("FROZEN: %s %s changed its license", repo, ev.Tag)
		m.Priority, m.Tags = notify.PriorityUrgent, []string{"rotating_light"}
		fmt.Fprintf(&b, "\nThe fork is frozen at %s and won't sync until you run: forkkeeper unfreeze --reason \"why\" %s\n", at, repo)
	case out.State.Frozen:
		m.Title = fmt.Sprintf("%s %s: license rule fired (repo is frozen)", repo, ev.Tag)
		fmt.Fprintf(&b, "\nThe fork stays frozen at %s.\n", out.State.FrozenRef)
	case out.Approval != nil && out.Approval.Status == store.ApprovalPending:
		verb := "needs approval"
		if out.Approval.Kind == store.ApprovalOverride {
			verb = "is blocked"
		}
		m.Title = fmt.Sprintf("%s %s %s (#%d)", repo, ev.Tag, verb, out.Approval.ID)
		m.Priority, m.Tags = notify.PriorityHigh, []string{"warning"}
	default:
		m.Title = fmt.Sprintf("%s %s: license changed", repo, ev.Tag)
	}

	if out.Backup != nil && len(out.Backup.Added)+len(out.Backup.Existing) > 0 {
		b.WriteString("The last good version's source is backed up.\n")
	}
	prURL := ""
	if s := out.Sync; s != nil && s.PRURL != "" {
		prURL = s.PRURL
		policy := s.MergePolicy
		switch rules.MergePolicy(policy) {
		case rules.MergeAuto:
			fmt.Fprintf(&b, "Sync PR #%d on %s will be merged once CI passes.\n", s.PRNumber, s.Fork)
		default:
			fmt.Fprintf(&b, "Sync PR #%d on %s is on hold (%s).\n", s.PRNumber, s.Fork, strings.ReplaceAll(policy, "_", " "))
		}
	}
	if len(out.Waiting) > 0 {
		var refs []string
		for _, w := range out.Waiting {
			refs = append(refs, fmt.Sprintf("%s (#%d)", w.Ref, w.ID))
		}
		fmt.Fprintf(&b, "Earlier versions still waiting for approval: %s.\n", strings.Join(refs, ", "))
	}
	if a := out.Approval; a != nil && a.Status == store.ApprovalPending {
		b.WriteString("\n" + notify.ApprovalHelp(a.ID, a.Kind, prURL != ""))
	}
	m.Body = strings.TrimSpace(b.String())
	if prURL != "" {
		m.Click = prURL
		m.Links = append(m.Links, notify.Link{Label: "Open PR", URL: prURL})
	}
	if ev.URL != "" {
		m.Links = append(m.Links, notify.Link{Label: "Release", URL: ev.URL})
	}
	return m
}

func prMessage(ev provider.Event, out *Outcome) notify.Message {
	s := out.Sync
	repo := ev.Repo.FullName()
	verb := "opened"
	if !s.PRCreated {
		verb = "updated to " + ev.Tag
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sync PR #%d on %s brings in upstream %s %s.", s.PRNumber, s.Fork, repo, ev.Tag)
	if r := out.Check.Report; r != nil && r.Declared != "" {
		fmt.Fprintf(&b, " License: %s.", r.Declared)
	}
	b.WriteString("\n\n")
	switch rules.MergePolicy(s.MergePolicy) {
	case rules.MergeAuto:
		b.WriteString("Fork Keeper merges it once CI passes.")
	default:
		fmt.Fprintf(&b, "It's on hold (%s).", strings.ReplaceAll(s.MergePolicy, "_", " "))
	}
	m := notify.Message{Kind: notify.KindPR, Repo: repo, Ref: ev.Tag, Priority: notify.PriorityDefault,
		Tags: []string{"arrows_counterclockwise"}, Title: fmt.Sprintf("%s %s: sync PR #%d %s", repo, ev.Tag, s.PRNumber, verb),
		Body: b.String(), Click: s.PRURL, Links: []notify.Link{{Label: "Open PR", URL: s.PRURL}}}
	if ev.URL != "" {
		m.Links = append(m.Links, notify.Link{Label: "Release", URL: ev.URL})
	}
	return m
}

func conflictMessage(ev provider.Event, out *Outcome) notify.Message {
	s := out.Sync
	body := fmt.Sprintf("Merging upstream %s %s into %s conflicts", ev.Repo.FullName(), ev.Tag, s.Fork)
	if len(s.Conflicts) > 0 {
		body += " in " + strings.Join(s.Conflicts, ", ")
	}
	body += ".\n\nResolve it by hand: forkkeeper status " + ev.Repo.FullName() + " shows the details."
	return notify.Message{Kind: notify.KindSync, Repo: ev.Repo.FullName(), Ref: ev.Tag, Priority: notify.PriorityDefault,
		Tags: []string{"x"}, Title: fmt.Sprintf("Merge conflict syncing %s %s", ev.Repo.FullName(), ev.Tag), Body: body,
		Click: ev.URL}
}
