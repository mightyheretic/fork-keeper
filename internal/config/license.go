package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/license"
)

// License configures how license changes are handled.
type License struct {
	// Allowlist is the set of licenses you accept. The "not-allowed"
	// pattern matches anything outside it.
	Allowlist []string `yaml:"allowlist"`
	// DefaultActions apply when licenses changed but no rule matched.
	// Default: [require_approval, notify].
	DefaultActions []string `yaml:"default_actions"`
	Rules          []Rule   `yaml:"rules"`
}

// Rule maps a license transition to actions.
type Rule struct {
	Name string `yaml:"name"`
	// Repos and Groups limit the rule's scope. With neither, it is global.
	// A repo rule beats a group rule, which beats a global rule.
	Repos  []string `yaml:"repos"`
	Groups []string `yaml:"groups"`
	// From matches licenses of the previous version. Default: [any].
	From []string `yaml:"from"`
	// To matches licenses added in the new version.
	To []string `yaml:"to"`
	// Removed matches licenses dropped in the new version. A rule needs
	// To, Removed, or both (either one matching is enough).
	Removed []string `yaml:"removed"`
	Actions []string `yaml:"actions"`
	// Priority breaks ties between equally specific rules (higher wins);
	// after that, the earlier rule in the file wins.
	Priority int `yaml:"priority"`
	// Level is "top" (default: top-level license files and manifests) or
	// "any" (also nested license files and file headers).
	Level string `yaml:"level"`
}

// Rule actions.
const (
	ActionFreeze            = "freeze"
	ActionBlockAutoMerge    = "block_auto_merge"
	ActionRequireApproval   = "require_approval"
	ActionFlagPR            = "flag_pr"
	ActionNotify            = "notify"
	ActionArchive           = "archive"
	ActionMaintenanceBranch = "maintenance_branch"
	// ActionAllow accepts the change explicitly (auto-merge stays on).
	ActionAllow = "allow"
)

var validActions = []string{
	ActionFreeze, ActionBlockAutoMerge, ActionRequireApproval, ActionFlagPR,
	ActionNotify, ActionArchive, ActionMaintenanceBranch, ActionAllow,
}

// License patterns besides SPDX IDs and globs.
const (
	PatternAny        = "any"
	PatternNotAllowed = "not-allowed" // not in the allowlist
	PatternUnknown    = "unknown"     // NONE or NOASSERTION
)

// Rule levels.
const (
	LevelTop = "top"
	LevelAny = "any"
)

func (c *Config) licenseDefaults() {
	if c.License.DefaultActions == nil {
		c.License.DefaultActions = []string{ActionRequireApproval, ActionNotify}
	}
	for i := range c.License.Rules {
		r := &c.License.Rules[i]
		if len(r.From) == 0 {
			r.From = []string{PatternAny}
		}
		if r.Level == "" {
			r.Level = LevelTop
		}
	}
}

func (c *Config) validateLicense(add func(string, ...any)) {
	l := c.License
	for _, id := range l.Allowlist {
		if !isLicenseID(id) {
			add("license.allowlist: unknown license %q", id)
		}
	}
	validateActions("license.default_actions", l.DefaultActions, add)

	upstreams := map[string]bool{}
	groups := map[string]bool{}
	for _, r := range c.Repos {
		upstreams[strings.ToLower(r.Upstream)] = true
		for _, g := range r.Groups {
			groups[g] = true
		}
	}

	names := map[string]bool{}
	for i, r := range l.Rules {
		p := fmt.Sprintf("license.rules[%d]", i)
		if r.Name == "" {
			add("%s.name is required", p)
		} else if names[r.Name] {
			add("%s.name: %q is used twice", p, r.Name)
		}
		names[r.Name] = true

		for _, repo := range r.Repos {
			if !upstreams[strings.ToLower(repo)] {
				add("%s.repos: %s is not an upstream in repos:", p, repo)
			}
		}
		for _, g := range r.Groups {
			if !groups[g] {
				add("%s.groups: no repo is in group %q", p, g)
			}
		}
		if len(r.To) == 0 && len(r.Removed) == 0 {
			add("%s: needs to, removed, or both", p)
		}
		for field, pats := range map[string][]string{"from": r.From, "to": r.To, "removed": r.Removed} {
			for _, pat := range pats {
				if err := validatePattern(pat, len(l.Allowlist) > 0); err != nil {
					add("%s.%s: %v", p, field, err)
				}
			}
		}
		validateActions(p+".actions", r.Actions, add)
		if r.Level != LevelTop && r.Level != LevelAny {
			add("%s.level: unknown level %q (want top or any)", p, r.Level)
		}
	}
}

func validateActions(field string, actions []string, add func(string, ...any)) {
	if len(actions) == 0 {
		add("%s: at least one action is required", field)
	}
	for _, a := range actions {
		if !slices.Contains(validActions, a) {
			add("%s: unknown action %q (want one of %s)", field, a, strings.Join(validActions, ", "))
		}
	}
	if slices.Contains(actions, ActionAllow) {
		for _, a := range []string{ActionFreeze, ActionBlockAutoMerge, ActionRequireApproval} {
			if slices.Contains(actions, a) {
				add("%s: %s contradicts %s", field, ActionAllow, a)
			}
		}
	}
}

func validatePattern(pat string, haveAllowlist bool) error {
	switch strings.ToLower(pat) {
	case PatternAny, PatternUnknown, "none":
		return nil
	case PatternNotAllowed:
		if !haveAllowlist {
			return fmt.Errorf("%q needs a license.allowlist", pat)
		}
		return nil
	}
	if strings.ContainsAny(pat, "*?[") {
		return nil
	}
	if !isLicenseID(pat) {
		return fmt.Errorf("unknown license %q (use an SPDX ID, a glob like GPL-*, any, not-allowed or unknown)", pat)
	}
	return nil
}

func isLicenseID(id string) bool { return license.Known(license.Normalize(id)) }
