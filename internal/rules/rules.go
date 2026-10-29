// Package rules decides what to do about a license change. Each rule maps
// a transition (licenses in the old version -> licenses added or removed in
// the new one) to actions. The most specific matching rule wins.
package rules

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/license"
)

// MergePolicy tells the merge engine how to treat the new version.
type MergePolicy string

const (
	// MergeAuto: open a PR and merge it once CI passes.
	MergeAuto MergePolicy = "auto"
	// MergeApproval: open a PR, but merge only after a human approves.
	MergeApproval MergePolicy = "approval_required"
	// MergeBlocked: open a PR, never merge it automatically.
	MergeBlocked MergePolicy = "blocked"
	// MergeFrozen: don't sync; the fork stays at its last good version.
	MergeFrozen MergePolicy = "frozen"
)

// Scopes, most specific last.
const (
	ScopeDefault = "default"
	ScopeGlobal  = "global"
	ScopeGroup   = "group"
	ScopeRepo    = "repo"
)

var specificity = map[string]int{ScopeGlobal: 1, ScopeGroup: 2, ScopeRepo: 3}

// Decision is the outcome of evaluating one version.
type Decision struct {
	// Rule is the winning rule's name, "default" when licenses changed but
	// no rule matched, or "" when there was nothing to decide.
	Rule        string      `json:"rule"`
	Scope       string      `json:"scope"`
	Actions     []string    `json:"actions"`
	MergePolicy MergePolicy `json:"merge_policy"`
	Reason      string      `json:"reason"`
	// Matched lists every rule that matched, winner first.
	Matched []string `json:"matched,omitempty"`
}

// Has reports whether the decision includes action.
func (d Decision) Has(action string) bool { return slices.Contains(d.Actions, action) }

// Engine evaluates the configured rules.
type Engine struct {
	allow    map[string]bool
	defaults []string
	escalate []string
	rules    []rule
	groupsOf func(repo string) []string
	subOf    func(repo string) string
}

type rule struct {
	config.Rule
	index int
	scope string
	repos map[string]bool
}

// New builds an engine from validated config.
func New(cfg *config.Config) *Engine {
	e := &Engine{
		allow:    map[string]bool{},
		defaults: cfg.License.DefaultActions,
		escalate: cfg.License.EscalationActions,
		groupsOf: func(repo string) []string {
			r, _ := cfg.RepoFor(repo)
			return r.Groups
		},
		subOf: func(repo string) string {
			if r, ok := cfg.RepoFor(repo); ok {
				return r.SubLicenses
			}
			return config.SubLicensesEscalate
		},
	}
	for _, id := range cfg.License.Allowlist {
		e.allow[license.Normalize(id)] = true
	}
	for i, r := range cfg.License.Rules {
		cr := rule{Rule: r, index: i, scope: ScopeGlobal, repos: map[string]bool{}}
		switch {
		case len(r.Repos) > 0:
			cr.scope = ScopeRepo
			for _, repo := range r.Repos {
				cr.repos[strings.ToLower(repo)] = true
			}
		case len(r.Groups) > 0:
			cr.scope = ScopeGroup
		}
		e.rules = append(e.rules, cr)
	}
	return e
}

// Scope of the built-in sub-license escalation.
const ScopeEscalation = "escalation"

// Evaluate decides what to do about the change from d.OldRef to d.NewRef
// for repo ("owner/name"). Rules run when the project's own (top-level)
// license changed. Sub-license changes (nested license files, file
// headers) depend on the repo's sub_licenses mode; see
// config.SubLicensesEscalate.
func (e *Engine) Evaluate(repo string, d *license.Diff) Decision {
	mode := e.subOf(repo)
	if !d.Changed {
		return Decision{MergePolicy: MergeAuto, Reason: "licenses unchanged"}
	}
	if !d.TopLevelChanged {
		switch mode {
		case config.SubLicensesIgnore:
			return Decision{MergePolicy: MergeAuto, Reason: "top-level license unchanged; sub-license changes ignored"}
		case config.SubLicensesEscalate:
			return e.escalation(d)
		}
	}
	t := transition{old: d.OldLicenses, added: d.Added, removed: d.Removed}
	if mode == config.SubLicensesAll {
		t.added = union(d.Added, d.Headers.Added, nestedAdded(d))
		t.removed = union(d.Removed, d.Headers.Removed)
	}

	var matched []rule
	var reasons []string
	for _, r := range e.rules {
		if !e.inScope(r, repo) {
			continue
		}
		if why, ok := e.match(r, t); ok {
			matched = append(matched, r)
			reasons = append(reasons, why)
		}
	}

	if len(matched) == 0 {
		return e.decide("default", ScopeDefault, e.defaults, nil,
			"licenses changed but no rule matched: "+d.Summary())
	}
	best := 0
	for i := range matched {
		if beats(matched[i], matched[best]) {
			best = i
		}
	}
	order := []string{matched[best].Name}
	for i, r := range matched {
		if i != best {
			order = append(order, r.Name)
		}
	}
	return e.decide(matched[best].Name, matched[best].scope, matched[best].Actions, order, reasons[best])
}

// escalation handles a version whose top-level license is unchanged: it
// acts only if a sub-license brings in a license that is new to the
// project and not on the allowlist (or unidentified).
func (e *Engine) escalation(d *license.Diff) Decision {
	var flagged []string
	for _, id := range d.SubAdded {
		if !e.allow[id] {
			flagged = append(flagged, id)
		}
	}
	if len(flagged) == 0 {
		return Decision{MergePolicy: MergeAuto,
			Reason: "top-level license unchanged; sub-license changes only use licenses already in the project or on the allowlist"}
	}
	return e.decide("sub-license-escalation", ScopeEscalation, e.escalate, nil,
		fmt.Sprintf("top-level license unchanged, but nested license files or file headers bring in %s, "+
			"new to the project and not on the allowlist: %s", strings.Join(flagged, ", "), d.Summary()))
}

// EvaluateFirst handles the first version seen of a repo: there is no
// previous version to compare with, so it only checks the allowlist.
func (e *Engine) EvaluateFirst(repo string, r *license.Report) Decision {
	if len(e.allow) == 0 {
		return Decision{MergePolicy: MergeAuto, Reason: "first version seen; no allowlist configured"}
	}
	var outside []string
	for _, id := range r.Licenses {
		if !e.allow[id] {
			outside = append(outside, id)
		}
	}
	if len(outside) == 0 {
		return Decision{MergePolicy: MergeAuto, Reason: "first version seen; licenses are on the allowlist"}
	}
	return e.decide("default", ScopeDefault, e.defaults, nil,
		fmt.Sprintf("first version seen is under %s, which is not on the allowlist", strings.Join(outside, ", ")))
}

func (e *Engine) decide(name, scope string, actions, matched []string, reason string) Decision {
	return Decision{
		Rule:        name,
		Scope:       scope,
		Actions:     slices.Clone(actions),
		MergePolicy: policyFor(actions),
		Reason:      reason,
		Matched:     matched,
	}
}

// policyFor maps actions to the strictest merge policy they imply.
func policyFor(actions []string) MergePolicy {
	switch {
	case slices.Contains(actions, config.ActionFreeze):
		return MergeFrozen
	case slices.Contains(actions, config.ActionBlockAutoMerge):
		return MergeBlocked
	case slices.Contains(actions, config.ActionRequireApproval):
		return MergeApproval
	}
	return MergeAuto
}

// beats reports whether a should win over b: more specific scope, then
// higher priority, then earlier in the file.
func beats(a, b rule) bool {
	if sa, sb := specificity[a.scope], specificity[b.scope]; sa != sb {
		return sa > sb
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.index < b.index
}

func (e *Engine) inScope(r rule, repo string) bool {
	switch r.scope {
	case ScopeRepo:
		return r.repos[strings.ToLower(repo)]
	case ScopeGroup:
		for _, g := range e.groupsOf(repo) {
			if slices.Contains(r.Groups, g) {
				return true
			}
		}
		return false
	}
	return true
}

type transition struct {
	old, added, removed []string
}

// match checks a rule against a transition and explains why it matched.
func (e *Engine) match(r rule, t transition) (string, bool) {
	from := e.matching(r.From, t.old)
	if len(from) == 0 && !(slices.Contains(r.From, config.PatternAny) && len(t.old) == 0) {
		return "", false
	}
	to := e.matching(r.To, t.added)
	removed := e.matching(r.Removed, t.removed)
	if len(to) == 0 && len(removed) == 0 {
		return "", false
	}
	var parts []string
	if len(to) > 0 {
		parts = append(parts, "added "+strings.Join(to, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	fromText := strings.Join(t.old, ", ")
	if fromText == "" {
		fromText = license.None
	}
	return fmt.Sprintf("rule %q: was %s, %s", r.Name, fromText, strings.Join(parts, "; ")), true
}

// matching returns the licenses in ids that any of the patterns match.
func (e *Engine) matching(patterns, ids []string) []string {
	var out []string
	for _, id := range ids {
		for _, p := range patterns {
			if e.matches(p, id) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func (e *Engine) matches(pattern, id string) bool {
	switch strings.ToLower(pattern) {
	case config.PatternAny:
		return true
	case config.PatternUnknown:
		return id == license.None || id == license.NoAssertion
	case "none":
		return id == license.None
	case config.PatternNotAllowed:
		return !e.allow[id]
	}
	if strings.ContainsAny(pattern, "*?[") {
		ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(id))
		return ok
	}
	return license.Normalize(pattern) == id
}

// nestedAdded returns licenses introduced by nested license files.
func nestedAdded(d *license.Diff) []string {
	var out []string
	for _, f := range d.Files {
		if f.Root || (f.Change != "added" && f.Change != "relicensed") {
			continue
		}
		var old []string
		if e, err := license.Parse(f.Old); err == nil && f.Old != "" {
			old = e.Licenses()
		}
		if e, err := license.Parse(f.New); err == nil {
			for _, id := range e.Licenses() {
				if !slices.Contains(old, id) {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

func union(sets ...[]string) []string {
	var out []string
	for _, s := range sets {
		for _, x := range s {
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Listed is a rule as it applies to one repo.
type Listed struct {
	config.Rule
	Scope string
}

// RulesFor lists the rules that apply to repo ("" for every rule), in the
// order they win: most specific scope, then priority, then file order.
func (e *Engine) RulesFor(repo string) []Listed {
	var rs []rule
	for _, r := range e.rules {
		if repo == "" || e.inScope(r, repo) {
			rs = append(rs, r)
		}
	}
	slices.SortStableFunc(rs, func(a, b rule) int {
		switch {
		case beats(a, b):
			return -1
		case beats(b, a):
			return 1
		}
		return 0
	})
	out := make([]Listed, len(rs))
	for i, r := range rs {
		out[i] = Listed{Rule: r.Rule, Scope: r.scope}
	}
	return out
}

// Defaults returns the default actions (licenses changed, no rule matched)
// and the sub-license escalation actions.
func (e *Engine) Defaults() (defaults, escalation []string) {
	return slices.Clone(e.defaults), slices.Clone(e.escalate)
}
