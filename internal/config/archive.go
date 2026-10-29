package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

// Archive configures source backups of upstream releases. Backups hold git
// history and tags only, never release assets.
type Archive struct {
	// Releases picks which upstream releases are backed up: major, minor
	// (default), all or none. The last version before a license change is
	// always backed up.
	Releases string `yaml:"releases"`
	// PushToFork copies backed-up refs to the repo's fork, under hidden
	// refs/forkkeeper/archive/ refs. Default true.
	PushToFork *bool `yaml:"push_to_fork"`
	// Mirrors are extra git remotes that receive every backed-up ref.
	Mirrors []Mirror `yaml:"mirrors"`
	// SoftwareHeritage asks Software Heritage to archive each upstream when
	// a release is backed up. Default true.
	SoftwareHeritage *bool `yaml:"software_heritage"`
	// SoftwareHeritageToken is an optional credential name holding a
	// Software Heritage API token, for higher rate limits.
	SoftwareHeritageToken string `yaml:"software_heritage_token"`
	// SoftwareHeritageURL is the API base. Default
	// https://archive.softwareheritage.org/api/1.
	SoftwareHeritageURL string `yaml:"software_heritage_url"`
}

// RepoArchive overrides Archive for one repo.
type RepoArchive struct {
	Releases         string   `yaml:"releases"`
	PushToFork       *bool    `yaml:"push_to_fork"`
	Mirrors          []Mirror `yaml:"mirrors"`
	SoftwareHeritage *bool    `yaml:"software_heritage"`
}

// Mirror is a git remote that receives backups.
type Mirror struct {
	// URL is an HTTPS git URL. {owner} and {name} are replaced with the
	// upstream's, so one mirror entry can cover every repo.
	URL string `yaml:"url"`
	// Token is the credential name of the token to push with.
	Token string `yaml:"token"`
	// Username goes with Token in HTTP Basic auth. Default x-access-token
	// (GitHub); Codeberg and Gitea want your user name, GitLab "oauth2".
	Username string `yaml:"username"`
	// Refs is hidden (default): refs/forkkeeper/archive/..., which never
	// triggers CI or shows up as a tag; or tags: plain tags, for a
	// dedicated backup repo.
	Refs string `yaml:"refs"`
}

// Release backup policies.
const (
	ReleasesMajor = "major"
	ReleasesMinor = "minor"
	ReleasesAll   = "all"
	ReleasesNone  = "none"
)

// Mirror ref layouts.
const (
	MirrorRefsHidden = "hidden"
	MirrorRefsTags   = "tags"
)

// DefaultSoftwareHeritageURL is Software Heritage's public API.
const DefaultSoftwareHeritageURL = "https://archive.softwareheritage.org/api/1"

func (c *Config) archiveDefaults() {
	a := &c.Archive
	if a.Releases == "" {
		a.Releases = ReleasesMinor
	}
	if a.SoftwareHeritageURL == "" {
		a.SoftwareHeritageURL = DefaultSoftwareHeritageURL
	}
	fill := func(ms []Mirror) {
		for i := range ms {
			if ms[i].Username == "" {
				ms[i].Username = "x-access-token"
			}
			if ms[i].Refs == "" {
				ms[i].Refs = MirrorRefsHidden
			}
		}
	}
	fill(a.Mirrors)
	for i := range c.Repos {
		fill(c.Repos[i].Archive.Mirrors)
	}
}

func (c *Config) validateArchive(add func(string, ...any)) {
	validPolicy := func(p, field string) {
		switch p {
		case "", ReleasesMajor, ReleasesMinor, ReleasesAll, ReleasesNone:
		default:
			add("%s: unknown policy %q (want major, minor, all or none)", field, p)
		}
	}
	validMirrors := func(ms []Mirror, field string) {
		for i, m := range ms {
			p := fmt.Sprintf("%s[%d]", field, i)
			u, err := url.Parse(strings.NewReplacer("{owner}", "o", "{name}", "n").Replace(m.URL))
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				add("%s.url: %q must be an https:// URL without credentials in it", p, m.URL)
			}
			if m.Token != "" {
				if err := credentials.ValidateName(m.Token); err != nil {
					add("%s.token: %v", p, err)
				}
			}
			if strings.ContainsAny(m.Username, ":\n") {
				add("%s.username: must not contain : or newlines", p)
			}
			if m.Refs != MirrorRefsHidden && m.Refs != MirrorRefsTags {
				add("%s.refs: unknown layout %q (want hidden or tags)", p, m.Refs)
			}
		}
	}
	a := c.Archive
	validPolicy(a.Releases, "archive.releases")
	validMirrors(a.Mirrors, "archive.mirrors")
	if a.SoftwareHeritageToken != "" {
		if err := credentials.ValidateName(a.SoftwareHeritageToken); err != nil {
			add("archive.software_heritage_token: %v", err)
		}
	}
	if u, err := url.Parse(a.SoftwareHeritageURL); err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		add("archive.software_heritage_url: %q is not a valid URL", a.SoftwareHeritageURL)
	}
	for i, r := range c.Repos {
		p := fmt.Sprintf("repos[%d].archive", i)
		validPolicy(r.Archive.Releases, p+".releases")
		validMirrors(r.Archive.Mirrors, p+".mirrors")
	}
}

// ArchiveSettings is the effective archive configuration for one repo.
type ArchiveSettings struct {
	Releases         string
	PushToFork       bool
	Mirrors          []Mirror
	SoftwareHeritage bool
}

// ArchiveFor returns r's archive settings: its overrides on top of the
// top-level ones, with mirror URLs filled in.
func (c *Config) ArchiveFor(r Repo) ArchiveSettings {
	s := ArchiveSettings{
		Releases:         c.Archive.Releases,
		PushToFork:       c.Archive.PushToFork == nil || *c.Archive.PushToFork,
		SoftwareHeritage: c.Archive.SoftwareHeritage == nil || *c.Archive.SoftwareHeritage,
		Mirrors:          c.MirrorsFor(r),
	}
	if r.Archive.Releases != "" {
		s.Releases = r.Archive.Releases
	}
	if r.Archive.PushToFork != nil {
		s.PushToFork = *r.Archive.PushToFork
	}
	if r.Archive.SoftwareHeritage != nil {
		s.SoftwareHeritage = *r.Archive.SoftwareHeritage
	}
	return s
}

// MirrorsFor returns the top-level mirrors followed by r's own, with
// {owner} and {name} replaced.
func (c *Config) MirrorsFor(r Repo) []Mirror {
	owner, name, _ := strings.Cut(r.Upstream, "/")
	fill := strings.NewReplacer("{owner}", owner, "{name}", name)
	var out []Mirror
	for _, m := range append(append([]Mirror{}, c.Archive.Mirrors...), r.Archive.Mirrors...) {
		m.URL = fill.Replace(m.URL)
		out = append(out, m)
	}
	return out
}
