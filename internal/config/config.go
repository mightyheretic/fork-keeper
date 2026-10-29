// Package config loads the Fork Keeper YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

// Config is the top-level configuration file.
type Config struct {
	DataDir     string      `yaml:"data_dir"`
	Database    Database    `yaml:"database"`
	Credentials Credentials `yaml:"credentials"`
	GitHub      GitHub      `yaml:"github"`
	Watcher     Watcher     `yaml:"watcher"`
	Repos       []Repo      `yaml:"repos"`
	License     License     `yaml:"license"`
	Merge       Merge       `yaml:"merge"`
	Archive     Archive     `yaml:"archive"`
	// Notifications configures where messages go.
	Notifications Notifications `yaml:"notifications"`
}

// Watcher configures upstream polling.
type Watcher struct {
	// PollInterval is how often each upstream is polled. Default 15m.
	PollInterval time.Duration `yaml:"poll_interval"`
	// IncludePrereleases queues pre-releases too. Repos can override it.
	IncludePrereleases bool `yaml:"include_prereleases"`
	// Auth is the GitHub auth profile used to poll upstreams, or
	// "anonymous". When empty, github.repo_auth is applied to the upstream
	// name, falling back to anonymous if nothing matches.
	Auth string `yaml:"auth"`
	// MainBranch turns on the default-branch early warning: each poll
	// checks whether the upstream's top-level license changed on its
	// default branch before a release ships it. Default true.
	MainBranch *bool `yaml:"main_branch"`
}

// Watch modes.
const (
	WatchReleases = "releases"
	WatchTags     = "tags"
)

// Anonymous is the Watcher.Auth value for unauthenticated polling.
const Anonymous = "anonymous"

// Repo is one fork Fork Keeper maintains.
type Repo struct {
	// Fork is your fork ("owner/name"). Without one, the upstream is only
	// watched and license-checked.
	Fork string `yaml:"fork"`
	// Upstream is the repo being followed ("owner/name").
	Upstream string `yaml:"upstream"`
	// Watch is "releases" (default) or "tags" for projects without releases.
	Watch string `yaml:"watch"`
	// IncludePrereleases overrides watcher.include_prereleases.
	IncludePrereleases *bool `yaml:"include_prereleases"`
	// Groups name sets of repos that license rules can target.
	Groups []string `yaml:"groups"`
	// SubLicenses is escalate (default), ignore or all: how nested license
	// files and file headers count. See SubLicensesEscalate.
	SubLicenses string `yaml:"sub_licenses"`

	// StableBranch is the fork branch PRs target. Default: the fork's
	// default branch.
	StableBranch string `yaml:"stable_branch"`
	// StagingBranch is where upstream is merged before the PR. Fork Keeper
	// owns it and force-pushes it. Default forkkeeper/staging.
	StagingBranch string `yaml:"staging_branch"`
	// MergeStrategy is merge (default), patch-stack or merge-only.
	MergeStrategy string `yaml:"merge_strategy"`

	// MainBranch overrides watcher.main_branch.
	MainBranch *bool `yaml:"main_branch"`
	// Archive overrides the top-level archive settings. Mirrors are added
	// to the top-level ones.
	Archive RepoArchive `yaml:"archive"`
}

// Merge strategies.
const (
	// StrategyMerge merges upstream with history; on conflicts it falls
	// back to re-applying the fork's patches on top of upstream.
	StrategyMerge = "merge"
	// StrategyPatchStack always re-applies the fork's patches on upstream.
	StrategyPatchStack = "patch-stack"
	// StrategyMergeOnly merges, and reports conflicts without a fallback.
	StrategyMergeOnly = "merge-only"
)

// DefaultStagingBranch is the staging branch when a repo doesn't set one.
const DefaultStagingBranch = "forkkeeper/staging"

// Merge configures the merge engine.
type Merge struct {
	// AutoMerge merges PRs whose merge policy is "auto" once CI passes.
	// Default true.
	AutoMerge *bool `yaml:"auto_merge"`
	// RequireCI holds auto-merge until at least one check has run. Default true.
	RequireCI *bool `yaml:"require_ci"`
	// CheckInterval is how often open sync PRs are checked. Default 5m.
	CheckInterval time.Duration `yaml:"check_interval"`
	// CommitterName and CommitterEmail sign merge and re-applied commits.
	CommitterName  string `yaml:"committer_name"`
	CommitterEmail string `yaml:"committer_email"`
}

// AutoMergeEnabled reports whether merge.auto_merge is on.
func (m Merge) AutoMergeEnabled() bool { return m.AutoMerge == nil || *m.AutoMerge }

// CIRequired reports whether merge.require_ci is on.
func (m Merge) CIRequired() bool { return m.RequireCI == nil || *m.RequireCI }

// WatchesMainBranch reports whether r's default branch is checked for
// license changes between releases.
func (c *Config) WatchesMainBranch(r Repo) bool {
	if r.MainBranch != nil {
		return *r.MainBranch
	}
	return c.Watcher.MainBranch == nil || *c.Watcher.MainBranch
}

// WantsPrereleases reports whether pre-releases of r should be queued.
func (c *Config) WantsPrereleases(r Repo) bool {
	if r.IncludePrereleases != nil {
		return *r.IncludePrereleases
	}
	return c.Watcher.IncludePrereleases
}

// PollAuthProfile returns the auth profile for polling upstream ("owner/name"),
// or "" for anonymous access.
func (c *Config) PollAuthProfile(upstream string) string {
	switch c.Watcher.Auth {
	case Anonymous:
		return ""
	case "":
		return c.GitHub.AuthProfileFor(upstream)
	default:
		return c.Watcher.Auth
	}
}

type Database struct {
	Driver string `yaml:"driver"`
	Path   string `yaml:"path"`
}

// Credentials selects and configures the credential backend.
type Credentials struct {
	Provider string `yaml:"provider"` // file | env | command
	File     struct {
		Dir string `yaml:"dir"`
	} `yaml:"file"`
	Env struct {
		Prefix string `yaml:"prefix"`
	} `yaml:"env"`
	Command struct {
		Argv    []string      `yaml:"argv"`
		Timeout time.Duration `yaml:"timeout"`
	} `yaml:"command"`
}

// DefaultAuthProfile is used for repos that match no repo_auth pattern.
const DefaultAuthProfile = "default"

// GitHub configures the GitHub provider.
type GitHub struct {
	APIURL string `yaml:"api_url"`
	// WebhookSecret is the credential name holding the webhook signing secret.
	WebhookSecret string `yaml:"webhook_secret"`
	// Auth maps a profile name to how Fork Keeper authenticates.
	Auth map[string]GitHubAuth `yaml:"auth"`
	// RepoAuth maps a repo pattern ("owner/name", "owner/*" or "*") to an
	// auth profile. The most specific pattern wins; unmatched repos use "default".
	RepoAuth map[string]string `yaml:"repo_auth"`
}

// Auth types.
const (
	AuthApp = "app"
	AuthPAT = "pat"
)

// GitHubAuth is one authentication profile.
type GitHubAuth struct {
	Type string `yaml:"type"` // app | pat

	// GitHub App fields.
	AppID int64 `yaml:"app_id"`
	// InstallationID is optional; when 0 it is looked up per repo.
	InstallationID int64 `yaml:"installation_id"`
	// PrivateKey is the credential name holding the App's PEM private key.
	PrivateKey string `yaml:"private_key"`

	// Token is the credential name holding a fine-grained personal access token.
	Token string `yaml:"token"`
}

// Load reads, expands and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse expands ${VAR} references in string values, decodes strictly,
// applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	cfg := &Config{}
	// An empty file has no document node and decodes to the zero config.
	if root.Kind != 0 {
		if err := expandNode(&root); err != nil {
			return nil, err
		}
		expanded, err := yaml.Marshal(&root)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(expanded))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, err
		}
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.DataDir == "" {
		c.DataDir = "/var/lib/forkkeeper"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.DataDir, "forkkeeper.db")
	}
	if c.Credentials.Provider == "" {
		c.Credentials.Provider = "file"
	}
	if c.Watcher.PollInterval == 0 {
		c.Watcher.PollInterval = 15 * time.Minute
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Watch == "" {
			r.Watch = WatchReleases
		}
		if r.StagingBranch == "" {
			r.StagingBranch = DefaultStagingBranch
		}
		if r.SubLicenses == "" {
			r.SubLicenses = SubLicensesEscalate
		}
		switch r.MergeStrategy {
		case "":
			r.MergeStrategy = StrategyMerge
		case "rebase":
			r.MergeStrategy = StrategyPatchStack
		}
	}
	c.licenseDefaults()
	c.archiveDefaults()
	if c.Merge.CheckInterval == 0 {
		c.Merge.CheckInterval = 5 * time.Minute
	}
	if c.Merge.CommitterName == "" {
		c.Merge.CommitterName = "Fork Keeper"
	}
	if c.Merge.CommitterEmail == "" {
		c.Merge.CommitterEmail = "forkkeeper@localhost"
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = "https://api.github.com"
	}
	if c.GitHub.WebhookSecret == "" {
		c.GitHub.WebhookSecret = "github_webhook_secret"
	}
	for name, a := range c.GitHub.Auth {
		if a.Type == AuthApp && a.PrivateKey == "" {
			a.PrivateKey = "github_app_private_key"
		}
		c.GitHub.Auth[name] = a
	}
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Database.Driver != "sqlite" {
		add("database.driver: only sqlite is supported")
	}

	switch c.Credentials.Provider {
	case "file":
		if c.Credentials.File.Dir == "" {
			add("credentials.file.dir is required for the file provider")
		}
	case "env":
	case "command":
		if len(c.Credentials.Command.Argv) == 0 {
			add("credentials.command.argv is required for the command provider")
		}
	default:
		add("credentials.provider: unknown provider %q (want file, env or command)", c.Credentials.Provider)
	}

	if u, err := url.Parse(c.GitHub.APIURL); err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		add("github.api_url: %q is not a valid URL", c.GitHub.APIURL)
	}
	if err := credentials.ValidateName(c.GitHub.WebhookSecret); err != nil {
		add("github.webhook_secret: %v", err)
	}

	for _, name := range slices.Sorted(maps.Keys(c.GitHub.Auth)) {
		a := c.GitHub.Auth[name]
		p := "github.auth." + name
		switch a.Type {
		case AuthApp:
			if a.AppID <= 0 {
				add("%s.app_id is required for a GitHub App", p)
			}
			if err := credentials.ValidateName(a.PrivateKey); err != nil {
				add("%s.private_key: %v", p, err)
			}
		case AuthPAT:
			if err := credentials.ValidateName(a.Token); err != nil {
				add("%s.token: %v", p, err)
			}
		default:
			add("%s.type: unknown type %q (want app or pat)", p, a.Type)
		}
	}

	for _, pattern := range slices.Sorted(maps.Keys(c.GitHub.RepoAuth)) {
		profile := c.GitHub.RepoAuth[pattern]
		if !validRepoPattern(pattern) {
			add("github.repo_auth: invalid pattern %q (want owner/name, owner/* or *)", pattern)
		}
		if _, ok := c.GitHub.Auth[profile]; !ok {
			add("github.repo_auth.%s: unknown auth profile %q", pattern, profile)
		}
	}

	if c.Watcher.PollInterval < time.Minute {
		add("watcher.poll_interval: must be at least 1m")
	}
	if a := c.Watcher.Auth; a != "" && a != Anonymous {
		if _, ok := c.GitHub.Auth[a]; !ok {
			add("watcher.auth: unknown auth profile %q", a)
		}
	}
	seen, forks := map[string]bool{}, map[string]bool{}
	for i, r := range c.Repos {
		p := fmt.Sprintf("repos[%d]", i)
		if !validRepoName(r.Upstream) {
			add("%s.upstream: %q is not owner/name", p, r.Upstream)
		} else if seen[strings.ToLower(r.Upstream)] {
			add("%s.upstream: %s is listed twice", p, r.Upstream)
		}
		seen[strings.ToLower(r.Upstream)] = true
		if r.Fork != "" && !validRepoName(r.Fork) {
			add("%s.fork: %q is not owner/name", p, r.Fork)
		} else if r.Fork != "" && forks[strings.ToLower(r.Fork)] {
			add("%s.fork: %s is listed twice", p, r.Fork)
		}
		forks[strings.ToLower(r.Fork)] = true
		if r.Watch != WatchReleases && r.Watch != WatchTags {
			add("%s.watch: unknown mode %q (want releases or tags)", p, r.Watch)
		}
		switch r.SubLicenses {
		case SubLicensesEscalate, SubLicensesIgnore, SubLicensesAll:
		default:
			add("%s.sub_licenses: unknown mode %q (want escalate, ignore or all)", p, r.SubLicenses)
		}
		switch r.MergeStrategy {
		case StrategyMerge, StrategyPatchStack, StrategyMergeOnly:
		default:
			add("%s.merge_strategy: unknown strategy %q (want merge, patch-stack or merge-only)", p, r.MergeStrategy)
		}
		for field, b := range map[string]string{"stable_branch": r.StableBranch, "staging_branch": r.StagingBranch} {
			if b != "" && !validBranch(b) {
				add("%s.%s: %q is not a valid branch name", p, field, b)
			}
		}
		if r.StableBranch != "" && r.StableBranch == r.StagingBranch {
			add("%s: staging_branch must differ from stable_branch", p)
		}
	}

	c.validateLicense(add)
	c.validateArchive(add)
	c.validateNotifications(add)
	if c.Merge.CheckInterval < 30*time.Second {
		add("merge.check_interval: must be at least 30s")
	}
	if strings.ContainsAny(c.Merge.CommitterName+c.Merge.CommitterEmail, "<>\n") {
		add("merge: committer_name and committer_email must not contain <, > or newlines")
	}

	return errors.Join(errs...)
}

// RepoFor returns the configured repo whose upstream is fullName.
func (c *Config) RepoFor(fullName string) (Repo, bool) {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Upstream, fullName) {
			return r, true
		}
	}
	return Repo{}, false
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func validRepoName(s string) bool { return repoName.MatchString(s) }

var branchName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*$`)

// validBranch is a conservative subset of git's ref name rules.
func validBranch(s string) bool {
	return branchName.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock")
}

var repoPattern = regexp.MustCompile(`^(\*|[A-Za-z0-9_.-]+/(\*|[A-Za-z0-9_.-]+))$`)

func validRepoPattern(p string) bool { return repoPattern.MatchString(p) }

// IsFork reports whether fullName is one of the configured forks.
func (c *Config) IsFork(fullName string) bool {
	for _, r := range c.Repos {
		if r.Fork != "" && strings.EqualFold(r.Fork, fullName) {
			return true
		}
	}
	return false
}

// AuthProfileFor returns the auth profile name for a repo ("owner/name").
// Exact matches beat "owner/*", which beats "*". It returns "" when nothing
// matches and there is no default profile.
func (g GitHub) AuthProfileFor(fullName string) string {
	owner, _, _ := strings.Cut(fullName, "/")
	for _, key := range []string{fullName, owner + "/*", "*"} {
		if p, ok := g.RepoAuth[key]; ok {
			return p
		}
	}
	if _, ok := g.Auth[DefaultAuthProfile]; ok {
		return DefaultAuthProfile
	}
	return ""
}

// CredentialNames lists every credential name the config refers to, so they
// can be checked at startup.
func (c *Config) CredentialNames() []string {
	seen := map[string]bool{c.GitHub.WebhookSecret: true}
	for _, a := range c.GitHub.Auth {
		switch a.Type {
		case AuthApp:
			seen[a.PrivateKey] = true
		case AuthPAT:
			seen[a.Token] = true
		}
	}
	if t := c.Archive.SoftwareHeritageToken; t != "" {
		seen[t] = true
	}
	for _, n := range c.Notifications.credentialNames() {
		seen[n] = true
	}
	for _, r := range c.Repos {
		for _, m := range c.MirrorsFor(r) {
			if m.Token != "" {
				seen[m.Token] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandNode replaces ${VAR} and ${VAR:-default} in scalar values (not keys
// or comments). An unset variable with no default is an error.
func expandNode(n *yaml.Node) error {
	var errs []error
	var walk func(n *yaml.Node, isKey bool)
	walk = func(n *yaml.Node, isKey bool) {
		switch n.Kind {
		case yaml.ScalarNode:
			if isKey {
				return
			}
			n.Value = envRef.ReplaceAllStringFunc(n.Value, func(ref string) string {
				m := envRef.FindStringSubmatch(ref)
				if v, ok := os.LookupEnv(m[1]); ok && v != "" {
					return v
				}
				if m[2] != "" {
					return m[3]
				}
				errs = append(errs, fmt.Errorf("line %d: environment variable %s is not set", n.Line, m[1]))
				return ""
			})
			// The expanded value may no longer match its original YAML type
			// (e.g. "${APP_ID}" -> 123). Let the decoder re-resolve it.
			if n.Style == 0 {
				n.Tag = ""
			}
		case yaml.MappingNode:
			for i, c := range n.Content {
				walk(c, i%2 == 0)
			}
		default:
			for _, c := range n.Content {
				walk(c, false)
			}
		}
	}
	walk(n, false)
	return errors.Join(errs...)
}
