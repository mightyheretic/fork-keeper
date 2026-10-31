// Package archive keeps source backups of upstream releases: git history
// and tags. Release files are copied to the fork by the releasefiles
// package, never kept here. Each upstream has a bare repository under
// <data_dir>/archive that only ever gains refs: a tag that upstream later
// moves or deletes keeps its original commit here. Backed-up refs are then
// pushed to the fork and any mirrors, and Software Heritage is asked to
// archive the upstream.
package archive

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/git"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/releasefiles"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/swh"
)

// Refs in the local archive repository. Upstream tags keep their names
// under refs/tags/; Fork Keeper's markers live outside it so they can never
// collide with an upstream tag.
const (
	LastGoodRef = "refs/forkkeeper/last-good"
	LastOpenRef = "refs/forkkeeper/last-open"
	headRef     = "refs/forkkeeper/upstream/head"
	// hiddenPrefix is where hidden-layout destinations keep backups. These
	// refs don't show up as tags or branches and never trigger CI.
	hiddenPrefix = "refs/forkkeeper/archive/"
)

// Names recorded for the markers.
const (
	LastGoodName = "forkkeeper/last-good"
	LastOpenName = "forkkeeper/last-open"
)

// Store is the subset of the store the archiver uses.
type Store interface {
	SaveArchivedRef(ctx context.Context, r store.ArchivedRef) error
	ArchivedRefs(ctx context.Context, provider, repo string) ([]store.ArchivedRef, error)
	SaveArchivePush(ctx context.Context, p store.ArchivePush) error
	LastSWHRequest(ctx context.Context, provider, repo string) (store.SWHRequest, bool, error)
	SaveSWHRequest(ctx context.Context, r store.SWHRequest) error
}

// Saver requests a Software Heritage archive of a repository URL.
type Saver interface {
	Save(ctx context.Context, originURL string) (swh.SaveRequest, error)
}

// FileCopier copies an upstream release's files to the fork.
type FileCopier interface {
	Copy(ctx context.Context, req releasefiles.Request) (releasefiles.Result, error)
}

// Archiver makes and distributes backups.
type Archiver struct {
	// Files copies release files when the settings ask for it; nil
	// disables that.
	Files FileCopier
	hosts map[string]provider.GitRemote // by provider name
	store Store
	git   *git.Runner
	dir   string
	creds credentials.Provider
	swh   Saver
	log   *slog.Logger
	// SWHInterval is the least time between routine Software Heritage
	// requests for one repo. License changes always request one.
	SWHInterval time.Duration
	now         func() time.Time
}

// New returns an archiver keeping repositories under dir. swh may be nil.
func New(hosts map[string]provider.GitRemote, st Store, runner *git.Runner, dir string,
	creds credentials.Provider, saver Saver, log *slog.Logger) *Archiver {
	return &Archiver{hosts: hosts, store: st, git: runner, dir: dir, creds: creds, swh: saver, log: log,
		SWHInterval: 24 * time.Hour, now: time.Now}
}

// Request is one backup.
type Request struct {
	Provider string
	Upstream provider.Repo
	// Fork receives the backups when Settings.PushToFork is set ("" for none).
	Fork     string
	Settings config.ArchiveSettings
	// Tags are upstream tags to back up.
	Tags []string
	// LastGood is a tag to back up and mark as the last version before a
	// license change.
	LastGood string
	// LastOpen is a default-branch commit to back up and mark as the last
	// one before a license change.
	LastOpen string
	Reason   string
	// Urgent requests a Software Heritage archive even if one was
	// requested recently.
	Urgent bool
}

// Result is what Backup did.
type Result struct {
	// Added lists refs backed up by this call; Existing those already
	// backed up (they are never re-fetched).
	Added    []string
	Existing []string
	// Missing lists tags upstream doesn't have (any more).
	Missing []string
	Pushes  []Push
	// SWH is the Software Heritage request made, if any.
	SWH *swh.SaveRequest
	// SWHNote says why no request was made, or why it failed.
	SWHNote string
	// Files has one entry per release whose files were copied to the fork.
	Files []releasefiles.Result
}

// Push is the outcome of pushing backups to one destination.
type Push struct {
	Destination string
	Refs        int
	Err         error
}

// Dir returns the local archive repository of upstream.
func (a *Archiver) Dir(providerName string, upstream provider.Repo) string {
	return filepath.Join(a.dir, providerName, upstream.Owner, upstream.Name+".git")
}

// Backup fetches the requested refs into the local archive, then pushes
// every backed-up ref of the repo to the fork and mirrors and asks
// Software Heritage to archive the upstream. Only local failures are
// returned; push and Software Heritage failures are recorded in Result and
// the store, and pushes are retried by the next backup.
func (a *Archiver) Backup(ctx context.Context, req Request) (Result, error) {
	var res Result
	host, ok := a.hosts[req.Provider]
	if !ok {
		return res, fmt.Errorf("no git remote for provider %q", req.Provider)
	}
	dir := a.Dir(req.Provider, req.Upstream)
	unlock, err := git.Lock(ctx, dir+".lock")
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := a.initRepo(ctx, dir); err != nil {
		return res, err
	}
	upstream := host.CloneURL(req.Upstream)
	auth, err := host.GitAuth(ctx, req.Upstream)
	if err != nil {
		return res, err
	}
	repo := req.Upstream.FullName()

	tags := append([]string{}, req.Tags...)
	if req.LastGood != "" {
		tags = append(tags, req.LastGood)
	}
	commits, err := a.fetchTags(ctx, dir, upstream, auth, tags, &res)
	if err != nil {
		return res, err
	}
	for _, t := range req.Tags {
		if c, ok := commits[t]; ok {
			if err := a.store.SaveArchivedRef(ctx, store.ArchivedRef{Provider: req.Provider, Repo: repo, Name: t,
				Kind: store.ArchiveRelease, CommitSHA: c, Reason: req.Reason}); err != nil {
				return res, err
			}
		}
	}
	if req.LastGood != "" {
		c, ok := commits[req.LastGood]
		if !ok {
			return res, fmt.Errorf("last good version %s is gone from %s and wasn't backed up before", req.LastGood, repo)
		}
		if err := a.mark(ctx, dir, req, LastGoodRef, LastGoodName, store.ArchiveLastGood, c); err != nil {
			return res, err
		}
		res.Added = append(res.Added, LastGoodName)
	}
	if req.LastOpen != "" {
		if err := a.ensureCommit(ctx, dir, upstream, auth, req.LastOpen); err != nil {
			return res, err
		}
		if err := a.mark(ctx, dir, req, LastOpenRef, LastOpenName, store.ArchiveLastOpen, req.LastOpen); err != nil {
			return res, err
		}
		res.Added = append(res.Added, LastOpenName)
	}

	res.Pushes = a.pushAll(ctx, dir, req)
	a.softwareHeritage(ctx, host, req, &res)
	a.copyFiles(ctx, req, tags, commits, &res)
	return res, nil
}

// copyFiles copies the release files of each backed-up tag to the fork.
// Failures are recorded, not returned: the source backup itself is done,
// and the next backup of the tag retries the files.
func (a *Archiver) copyFiles(ctx context.Context, req Request, tags []string, commits map[string]string, res *Result) {
	if a.Files == nil || !req.Settings.ReleaseFiles || req.Fork == "" {
		return
	}
	fork, err := provider.ParseRepo(req.Fork)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, t := range tags {
		c, ok := commits[t]
		if !ok || seen[t] {
			continue
		}
		seen[t] = true
		r, err := a.Files.Copy(ctx, releasefiles.Request{Provider: req.Provider, Upstream: req.Upstream, Fork: fork,
			Tag: t, Commit: c, Public: req.Settings.ReleaseFilesPublic})
		if err != nil {
			r.Note = err.Error()
			a.log.Warn("could not copy release files", "repo", req.Upstream.FullName(), "tag", t, "err", err)
		}
		res.Files = append(res.Files, r)
	}
}

func (a *Archiver) initRepo(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, err := a.git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"init", "--bare", "-q"}})
	return err
}

// fetchTags fetches tags not yet in the archive and returns the commit of
// every requested tag that is now backed up. An existing tag is never
// updated, so a tag upstream moved later keeps the commit first backed up.
func (a *Archiver) fetchTags(ctx context.Context, dir, url string, auth credentials.Secret, tags []string, res *Result) (map[string]string, error) {
	commits := map[string]string{}
	var want []string
	seen := map[string]bool{}
	for _, t := range tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		if _, err := a.git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"check-ref-format", "refs/tags/" + t}}); err != nil {
			return nil, fmt.Errorf("%w: %q is not a valid tag name", provider.ErrRefNotFound, t)
		}
		if c, err := a.commitOf(ctx, dir, "refs/tags/"+t); err == nil {
			commits[t] = c
			res.Existing = append(res.Existing, t)
			continue
		}
		want = append(want, t)
	}
	if len(want) == 0 {
		return commits, nil
	}

	// One fetch for all of them; if a tag is missing upstream, the fetch
	// fails as a whole, so fall back to one fetch per tag.
	err := a.fetch(ctx, dir, url, auth, tagSpecs(want)...)
	if isMissingRef(err) {
		for _, t := range want {
			if err := a.fetch(ctx, dir, url, auth, tagSpecs([]string{t})...); isMissingRef(err) {
				res.Missing = append(res.Missing, t)
			} else if err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	for _, t := range want {
		c, err := a.commitOf(ctx, dir, "refs/tags/"+t)
		if err != nil {
			continue // missing upstream
		}
		commits[t] = c
		res.Added = append(res.Added, t)
	}
	return commits, nil
}

func tagSpecs(tags []string) []string {
	specs := make([]string, len(tags))
	for i, t := range tags {
		// No "+": never overwrite a tag already backed up.
		specs[i] = "refs/tags/" + t + ":refs/tags/" + t
	}
	return specs
}

func (a *Archiver) fetch(ctx context.Context, dir, url string, auth credentials.Secret, specs ...string) error {
	args := append([]string{"fetch", "--no-tags", "--no-write-fetch-head", "--quiet", url}, specs...)
	_, err := a.git.Run(ctx, git.Cmd{Dir: dir, Auth: auth, Args: args})
	return err
}

func isMissingRef(err error) bool {
	var gerr *git.Error
	return errors.As(err, &gerr) && strings.Contains(gerr.Stderr, "couldn't find remote ref")
}

func (a *Archiver) commitOf(ctx context.Context, dir, ref string) (string, error) {
	out, err := a.git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"}})
	return strings.TrimSpace(out), err
}

// ensureCommit makes sure commit is in the archive, fetching the upstream
// default branch if needed.
func (a *Archiver) ensureCommit(ctx context.Context, dir, url string, auth credentials.Secret, commit string) error {
	if _, err := a.commitOf(ctx, dir, commit); err == nil {
		return nil
	}
	if err := a.fetch(ctx, dir, url, auth, "+HEAD:"+headRef); err != nil {
		return fmt.Errorf("fetch default branch: %w", err)
	}
	if _, err := a.commitOf(ctx, dir, commit); err != nil {
		return fmt.Errorf("commit %s is not on the upstream default branch", commit)
	}
	return nil
}

func (a *Archiver) mark(ctx context.Context, dir string, req Request, ref, name, kind, commit string) error {
	if _, err := a.git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"update-ref", ref, commit}}); err != nil {
		return err
	}
	return a.store.SaveArchivedRef(ctx, store.ArchivedRef{Provider: req.Provider, Repo: req.Upstream.FullName(),
		Name: name, Kind: kind, CommitSHA: commit, Reason: req.Reason})
}

// destination is somewhere backups are pushed.
type destination struct {
	name   string // for logs and the store
	url    string
	auth   credentials.Secret
	layout string // config.MirrorRefsHidden or MirrorRefsTags
}

// pushAll pushes every backed-up ref of the repo to each destination.
func (a *Archiver) pushAll(ctx context.Context, dir string, req Request) []Push {
	dests, errs := a.destinations(ctx, req)
	var out []Push
	for _, p := range errs {
		out = append(out, p)
		a.recordPush(ctx, req, p)
	}
	if len(dests) == 0 {
		return out
	}
	refs, err := a.localRefs(ctx, dir)
	if err != nil {
		for _, d := range dests {
			p := Push{Destination: d.name, Err: err}
			out = append(out, p)
			a.recordPush(ctx, req, p)
		}
		return out
	}
	for _, d := range dests {
		p := Push{Destination: d.name, Refs: len(refs)}
		if len(refs) > 0 {
			p.Err = a.push(ctx, dir, d, refs)
		}
		out = append(out, p)
		a.recordPush(ctx, req, p)
		if p.Err != nil {
			a.log.Warn("pushing backups failed; will retry with the next backup", "repo", req.Upstream.FullName(),
				"destination", d.name, "err", p.Err)
		} else {
			a.log.Info("backups pushed", "repo", req.Upstream.FullName(), "destination", d.name, "refs", len(refs))
		}
	}
	return out
}

func (a *Archiver) recordPush(ctx context.Context, req Request, p Push) {
	rec := store.ArchivePush{Provider: req.Provider, Repo: req.Upstream.FullName(), Destination: p.Destination,
		OK: p.Err == nil, Refs: p.Refs}
	if p.Err != nil {
		rec.Error = p.Err.Error()
	}
	if err := a.store.SaveArchivePush(ctx, rec); err != nil {
		a.log.Error("record archive push", "err", err)
	}
}

func (a *Archiver) destinations(ctx context.Context, req Request) ([]destination, []Push) {
	var dests []destination
	var failed []Push
	if req.Settings.PushToFork && req.Fork != "" {
		host := a.hosts[req.Provider]
		fork, err := provider.ParseRepo(req.Fork)
		name := "fork:" + req.Fork
		if err != nil {
			failed = append(failed, Push{Destination: name, Err: err})
		} else if auth, err := host.GitAuth(ctx, fork); err != nil {
			failed = append(failed, Push{Destination: name, Err: err})
		} else {
			dests = append(dests, destination{name: name, url: host.CloneURL(fork), auth: auth, layout: config.MirrorRefsHidden})
		}
	}
	for _, m := range req.Settings.Mirrors {
		d := destination{name: "mirror:" + m.URL, url: m.URL, layout: m.Refs}
		if m.Token != "" {
			tok, err := a.creds.Get(ctx, m.Token)
			if err != nil {
				failed = append(failed, Push{Destination: d.name, Err: err})
				continue
			}
			basic := base64.StdEncoding.EncodeToString([]byte(m.Username + ":" + tok.Reveal()))
			d.auth = credentials.NewSecret([]byte("Basic " + basic))
		}
		dests = append(dests, d)
	}
	return dests, failed
}

// localRefs lists the archive's tags and markers.
func (a *Archiver) localRefs(ctx context.Context, dir string) ([]string, error) {
	return a.git.Lines(ctx, git.Cmd{Dir: dir, Args: []string{"for-each-ref", "--format=%(refname)",
		"refs/tags/", LastGoodRef, LastOpenRef}})
}

// remoteRef maps a local archive ref to its name at a destination.
func remoteRef(local, layout string) string {
	name := strings.TrimPrefix(local, "refs/")
	if layout == config.MirrorRefsTags {
		if strings.HasPrefix(local, "refs/tags/") {
			return local
		}
		return "refs/tags/" + strings.TrimPrefix(local, "refs/") // refs/tags/forkkeeper/last-good
	}
	return hiddenPrefix + name // refs/forkkeeper/archive/tags/v1.0, .../forkkeeper/last-good
}

func (a *Archiver) push(ctx context.Context, dir string, d destination, refs []string) error {
	args := []string{"push", "--quiet", "--no-verify", d.url}
	for _, r := range refs {
		spec := r + ":" + remoteRef(r, d.layout)
		if !strings.HasPrefix(r, "refs/tags/") {
			spec = "+" + spec // markers move; tags never do
		}
		args = append(args, spec)
	}
	_, err := a.git.Run(ctx, git.Cmd{Dir: dir, Auth: d.auth, Args: args})
	var gerr *git.Error
	if errors.As(err, &gerr) && (strings.Contains(gerr.Stderr, "workflow") && strings.Contains(gerr.Stderr, "scope") ||
		strings.Contains(gerr.Stderr, "without `workflows` permission")) {
		return fmt.Errorf("rejected: the backups include GitHub Actions workflow changes, which needs the "+
			"workflows permission (PAT or App: Workflows read & write): %w", err)
	}
	return err
}

// softwareHeritage asks Software Heritage to archive the upstream, at most
// once per SWHInterval unless the request is urgent.
func (a *Archiver) softwareHeritage(ctx context.Context, host provider.GitRemote, req Request, res *Result) {
	if a.swh == nil || !req.Settings.SoftwareHeritage {
		res.SWHNote = "disabled"
		return
	}
	repo := req.Upstream.FullName()
	if !req.Urgent {
		last, ok, err := a.store.LastSWHRequest(ctx, req.Provider, repo)
		if err != nil {
			res.SWHNote = err.Error()
			return
		}
		if ok && last.Status != "error" && a.now().Sub(last.RequestedAt) < a.SWHInterval {
			res.SWHNote = "requested recently (" + last.RequestedAt.Format(time.RFC3339) + ")"
			return
		}
	}
	origin := strings.TrimSuffix(host.CloneURL(req.Upstream), ".git")
	rec := store.SWHRequest{Provider: req.Provider, Repo: repo, OriginURL: origin, Reason: req.Reason}
	sr, err := a.swh.Save(ctx, origin)
	if err != nil {
		rec.Status, rec.Reason = "error", err.Error()
		res.SWHNote = err.Error()
		a.log.Warn("Software Heritage request failed", "repo", repo, "err", err)
	} else {
		rec.RequestID, rec.Status, rec.TaskStatus = sr.ID, sr.Status, sr.TaskStatus
		res.SWH = &sr
		a.log.Info("Software Heritage archive requested", "repo", repo, "origin", origin, "status", sr.Status, "task", sr.TaskStatus)
	}
	if err := a.store.SaveSWHRequest(ctx, rec); err != nil {
		a.log.Error("record Software Heritage request", "err", err)
	}
}

// FindLastOpen returns the last default-branch commit before the files in
// paths changed between base and head: the first-parent ancestor just
// before the first commit in base..head that touched them. bases are tried
// in order (a commit SHA or a tag name); it falls back to the first base
// that exists when no commit touched paths.
func (a *Archiver) FindLastOpen(ctx context.Context, providerName string, upstream provider.Repo, head string, bases, paths []string) (string, error) {
	host, ok := a.hosts[providerName]
	if !ok {
		return "", fmt.Errorf("no git remote for provider %q", providerName)
	}
	dir := a.Dir(providerName, upstream)
	unlock, err := git.Lock(ctx, dir+".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := a.initRepo(ctx, dir); err != nil {
		return "", err
	}
	url := host.CloneURL(upstream)
	auth, err := host.GitAuth(ctx, upstream)
	if err != nil {
		return "", err
	}
	if err := a.ensureCommit(ctx, dir, url, auth, head); err != nil {
		return "", err
	}

	var fallback string
	for _, base := range bases {
		if base == "" {
			continue
		}
		c, err := a.commitOf(ctx, dir, base)
		if err != nil && !isSHAish(base) {
			// A release tag: back it up locally so it can serve as the base.
			if a.fetch(ctx, dir, url, auth, tagSpecs([]string{base})...) == nil {
				c, err = a.commitOf(ctx, dir, "refs/tags/"+base)
			}
		}
		if err != nil {
			continue
		}
		if fallback == "" {
			fallback = c
		}
		args := []string{"log", "--first-parent", "--reverse", "--format=%H", c + ".." + head, "--"}
		args = append(args, paths...)
		lines, err := a.git.Lines(ctx, git.Cmd{Dir: dir, Args: args})
		if err != nil || len(lines) == 0 {
			continue
		}
		parent, err := a.commitOf(ctx, dir, lines[0]+"^1")
		if err != nil {
			return "", fmt.Errorf("parent of %s: %w", lines[0], err)
		}
		return parent, nil
	}
	if fallback == "" {
		return "", errors.New("no base commit to compare with")
	}
	return fallback, nil
}

func isSHAish(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	_, err := strconv.ParseUint(s[:7], 16, 64)
	return err == nil && strings.Trim(s, "0123456789abcdef") == ""
}

// RemoteTags lists upstream's tags without fetching anything.
func (a *Archiver) RemoteTags(ctx context.Context, providerName string, upstream provider.Repo) ([]string, error) {
	host, ok := a.hosts[providerName]
	if !ok {
		return nil, fmt.Errorf("no git remote for provider %q", providerName)
	}
	auth, err := host.GitAuth(ctx, upstream)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, err
	}
	lines, err := a.git.Lines(ctx, git.Cmd{Dir: a.dir, Auth: auth,
		Args: []string{"ls-remote", "--tags", "--refs", host.CloneURL(upstream)}})
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, l := range lines {
		if _, ref, ok := strings.Cut(l, "\t"); ok {
			tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	return tags, nil
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// Wants reports whether policy backs up the release tagged tag:
//   - major: X.0.0, and 0.Y.0 (before 1.0 a minor bump is the major one)
//   - minor: X.Y.0
//   - all: every release
//   - none: nothing
//
// Pre-releases are never wanted, and tags without a version number only
// with all.
func Wants(policy, tag string) bool {
	if provider.LooksLikePrerelease(tag) {
		return false
	}
	switch policy {
	case config.ReleasesAll:
		return true
	case config.ReleasesNone:
		return false
	}
	m := versionRe.FindStringSubmatch(tag)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch := 0
	if m[3] != "" {
		patch, _ = strconv.Atoi(m[3])
	}
	if patch != 0 {
		return false
	}
	if policy == config.ReleasesMajor {
		return minor == 0 || major == 0
	}
	return policy == config.ReleasesMinor
}
