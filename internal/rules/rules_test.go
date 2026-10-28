package rules

import (
	"slices"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/license"
)

const baseConfig = `
credentials: {provider: env}
repos:
  - upstream: acme/app
    groups: [databases]
  - upstream: acme/db
    groups: [databases]
  - upstream: other/lib
license:
  allowlist: [MIT, Apache-2.0, BSD-3-Clause, GPL-2.0-only]
  default_actions: [require_approval]
  rules:
`

func engine(t *testing.T, rules string) *Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(baseConfig + rules))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg)
}

// diff builds a changed Diff from old and new top-level license sets.
func diff(old, cur []string) *license.Diff {
	d := &license.Diff{OldRef: "v1", NewRef: "v2", Changed: true, OldLicenses: old, NewLicenses: cur,
		OldDeclared: strings.Join(old, " AND "), NewDeclared: strings.Join(cur, " AND ")}
	for _, id := range cur {
		if !slices.Contains(old, id) {
			d.Added = append(d.Added, id)
		}
	}
	for _, id := range old {
		if !slices.Contains(cur, id) {
			d.Removed = append(d.Removed, id)
		}
	}
	return d
}

const relicenseRule = `
    - name: relicense
      from: [MIT, Apache-2.0]
      to: [BUSL-1.1, SSPL-1.0]
      actions: [freeze, notify]
`

func TestUnchanged(t *testing.T) {
	d := engine(t, relicenseRule).Evaluate("acme/app", &license.Diff{Changed: false})
	if d.Rule != "" || d.MergePolicy != MergeAuto || len(d.Actions) != 0 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestGlobalRelicense(t *testing.T) {
	d := engine(t, relicenseRule).Evaluate("other/lib", diff([]string{"MIT"}, []string{"BUSL-1.1"}))
	if d.Rule != "relicense" || d.Scope != ScopeGlobal || d.MergePolicy != MergeFrozen || !d.Has("notify") {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Reason, "was MIT, added BUSL-1.1") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestCompoundAdditionAndRemoval(t *testing.T) {
	e := engine(t, relicenseRule+`
    - name: gpl-dropped
      removed: [GPL-*]
      actions: [notify]
`)
	// MIT -> MIT OR BUSL: a license added alongside the old one still fires.
	if d := e.Evaluate("other/lib", diff([]string{"MIT"}, []string{"BUSL-1.1", "MIT"})); d.Rule != "relicense" {
		t.Fatalf("added alongside: %+v", d)
	}
	// Dropping an alternative only matches removed: rules.
	d := e.Evaluate("other/lib", diff([]string{"GPL-2.0-only", "MIT"}, []string{"MIT"}))
	if d.Rule != "gpl-dropped" || d.MergePolicy != MergeAuto {
		t.Fatalf("removal: %+v", d)
	}
}

func TestFromMustMatch(t *testing.T) {
	d := engine(t, relicenseRule).Evaluate("other/lib", diff([]string{"GPL-2.0-only"}, []string{"BUSL-1.1"}))
	if d.Rule != "default" || d.Scope != ScopeDefault || d.MergePolicy != MergeApproval {
		t.Fatalf("decision = %+v", d)
	}
}

func TestScopePrecedence(t *testing.T) {
	e := engine(t, relicenseRule+`
    - name: databases-ok
      groups: [databases]
      to: [SSPL-1.0]
      actions: [require_approval]
    - name: app-exception
      repos: [acme/app]
      to: [any]
      actions: [allow]
`)
	sspl := diff([]string{"MIT"}, []string{"SSPL-1.0"})
	for repo, want := range map[string]string{
		"acme/app":  "app-exception", // repo beats group and global
		"acme/db":   "databases-ok",  // group beats global
		"other/lib": "relicense",     // only the global rule applies
	} {
		d := e.Evaluate(repo, sspl)
		if d.Rule != want {
			t.Errorf("%s: rule = %q, want %q (matched %v)", repo, d.Rule, want, d.Matched)
		}
	}
	d := e.Evaluate("acme/app", sspl)
	if d.MergePolicy != MergeAuto || !slices.Equal(d.Matched, []string{"app-exception", "relicense", "databases-ok"}) {
		t.Fatalf("acme/app decision = %+v", d)
	}
}

func TestPriorityThenFileOrder(t *testing.T) {
	rules := `
    - name: first
      to: [BUSL-1.1]
      actions: [notify]
    - name: second
      to: [BUSL-1.1]
      actions: [freeze]
`
	if d := engine(t, rules).Evaluate("other/lib", diff([]string{"MIT"}, []string{"BUSL-1.1"})); d.Rule != "first" {
		t.Fatalf("file order: %q", d.Rule)
	}
	rules += "      priority: 5\n"
	if d := engine(t, rules).Evaluate("other/lib", diff([]string{"MIT"}, []string{"BUSL-1.1"})); d.Rule != "second" {
		t.Fatalf("priority: %q", d.Rule)
	}
}

func TestSpecialPatterns(t *testing.T) {
	e := engine(t, `
    - name: lost
      to: [unknown]
      actions: [block_auto_merge]
    - name: outside
      to: [not-allowed]
      actions: [require_approval]
    - name: gpl-family
      from: [GPL-*]
      to: [GPL-*]
      actions: [notify]
`)
	for _, tc := range []struct {
		old, cur []string
		want     string
		policy   MergePolicy
	}{
		{[]string{"MIT"}, []string{license.NoAssertion}, "lost", MergeBlocked},
		{[]string{"MIT"}, []string{license.None}, "lost", MergeBlocked},
		{[]string{"MIT"}, []string{"Elastic-2.0"}, "outside", MergeApproval},
		{[]string{"GPL-2.0-only"}, []string{"GPL-3.0-only"}, "outside", MergeApproval}, // GPL-3.0 isn't allowed; earlier rule wins
		{[]string{"MIT"}, []string{"Apache-2.0"}, "default", MergeApproval},
	} {
		d := e.Evaluate("other/lib", diff(tc.old, tc.cur))
		if d.Rule != tc.want || d.MergePolicy != tc.policy {
			t.Errorf("%v -> %v: rule=%q policy=%s, want %q %s", tc.old, tc.cur, d.Rule, d.MergePolicy, tc.want, tc.policy)
		}
	}
}

func TestLevelAnyIncludesHeadersAndNestedFiles(t *testing.T) {
	e := engine(t, `
    - name: top-only
      to: [BUSL-1.1]
      actions: [freeze]
    - name: deep
      to: [BUSL-1.1]
      level: any
      actions: [require_approval]
`)
	// Top-level stays MIT; BUSL appears only in file headers.
	d := &license.Diff{Changed: true, OldLicenses: []string{"MIT"}, NewLicenses: []string{"MIT"},
		OldDeclared: "MIT", NewDeclared: "MIT"}
	d.Headers.Added = []string{"BUSL-1.1"}
	if got := e.Evaluate("other/lib", d); got.Rule != "deep" {
		t.Fatalf("headers: %+v", got)
	}
	// Or in a new nested license file (ee/LICENSE).
	d = &license.Diff{Changed: true, OldLicenses: []string{"MIT"}, NewLicenses: []string{"MIT"},
		OldDeclared: "MIT", NewDeclared: "MIT",
		Files: []license.FileChange{{Path: "ee/LICENSE", Change: "added", New: "BUSL-1.1"}}}
	if got := e.Evaluate("other/lib", d); got.Rule != "deep" {
		t.Fatalf("nested file: %+v", got)
	}
}

func TestEvaluateFirst(t *testing.T) {
	e := engine(t, relicenseRule)
	if d := e.EvaluateFirst("other/lib", &license.Report{Licenses: []string{"MIT"}}); d.Rule != "" || d.MergePolicy != MergeAuto {
		t.Fatalf("allowed first version: %+v", d)
	}
	d := e.EvaluateFirst("other/lib", &license.Report{Licenses: []string{"BUSL-1.1", "MIT"}})
	if d.Rule != "default" || d.MergePolicy != MergeApproval || !strings.Contains(d.Reason, "BUSL-1.1") {
		t.Fatalf("first version outside allowlist: %+v", d)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, rules := range map[string]string{
		"unknown action":      "    - {name: a, to: [MIT], actions: [explode]}\n",
		"no actions":          "    - {name: a, to: [MIT], actions: []}\n",
		"unknown license":     "    - {name: a, to: [Apahce-2.0], actions: [notify]}\n",
		"allow contradiction": "    - {name: a, to: [MIT], actions: [allow, freeze]}\n",
		"no to or removed":    "    - {name: a, from: [MIT], actions: [notify]}\n",
		"unknown repo":        "    - {name: a, repos: [nobody/x], to: [MIT], actions: [notify]}\n",
		"unknown group":       "    - {name: a, groups: [nope], to: [MIT], actions: [notify]}\n",
		"duplicate name":      "    - {name: a, to: [MIT], actions: [notify]}\n    - {name: a, to: [ISC], actions: [notify]}\n",
		"missing name":        "    - {to: [MIT], actions: [notify]}\n",
		"bad level":           "    - {name: a, to: [MIT], actions: [notify], level: deep}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Parse([]byte(baseConfig + rules)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
	noAllow := "credentials: {provider: env}\nlicense:\n  rules:\n    - {name: a, to: [not-allowed], actions: [notify]}\n"
	if _, err := config.Parse([]byte(noAllow)); err == nil {
		t.Fatal("not-allowed without an allowlist should fail")
	}
	// Aliases are accepted: BSL-1.1 means BUSL-1.1.
	if _, err := config.Parse([]byte(baseConfig + "    - {name: a, to: [BSL-1.1], actions: [notify]}\n")); err != nil {
		t.Fatal(err)
	}
}
