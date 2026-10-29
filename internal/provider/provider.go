// Package provider defines the provider-neutral types that the Fork Keeper core
// works with. Each git host (GitHub, GitLab, Forgejo, ...) lives in its own
// subpackage and implements these interfaces; nothing outside those
// subpackages may import host-specific code.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

// EventKind is the normalized type of a webhook event.
type EventKind string

const (
	// KindRelease is a newly published release (including pre-releases).
	KindRelease EventKind = "release"
	// KindTag is a newly created tag that may not have a release attached.
	KindTag EventKind = "tag"
	// KindPing is a connectivity check sent when a webhook is first configured.
	KindPing EventKind = "ping"
	// KindIgnored is any event Fork Keeper does not act on.
	KindIgnored EventKind = "ignored"
)

// Repo identifies a repository on a provider.
type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	// CloneURL is the HTTPS clone URL reported by the provider.
	CloneURL string `json:"clone_url,omitempty"`
}

// FullName returns "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Event is a verified, normalized webhook delivery.
type Event struct {
	Provider string `json:"provider"`
	// DeliveryID is the provider's unique ID for this delivery. Redeliveries of
	// the same event carry the same ID, so it is the idempotency key.
	DeliveryID string    `json:"delivery_id"`
	Kind       EventKind `json:"kind"`
	Repo       Repo      `json:"repo"`
	Tag        string    `json:"tag,omitempty"`
	// ReleaseName is the human-readable release title, if any.
	ReleaseName string `json:"release_name,omitempty"`
	Prerelease  bool   `json:"prerelease,omitempty"`
	// URL links to the release or tag page, when known.
	URL string `json:"url,omitempty"`
	// PublishedAt is when upstream published the release, when known.
	PublishedAt time.Time `json:"published_at,omitzero"`
	// Source is how the event was discovered: SourceWebhook or SourcePoll.
	Source     string    `json:"source,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

// Event sources.
const (
	SourceWebhook = "webhook"
	SourcePoll    = "poll"
)

// ParseRepo parses "owner/name".
func ParseRepo(fullName string) (Repo, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("invalid repo %q: want owner/name", fullName)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// Ref is a release or tag listed by a provider.
type Ref struct {
	Kind        EventKind // KindRelease or KindTag
	Name        string    // tag name
	Title       string    // release title, if any
	Prerelease  bool
	PublishedAt time.Time // zero for plain tags
	URL         string
}

// RefList is one page of refs from a poll.
type RefList struct {
	// Refs are newest first when the provider can order them.
	Refs []Ref
	// ETag identifies this response for the next conditional request.
	ETag string
	// NotModified means the ETag matched and Refs is empty.
	NotModified bool
	RateLimit   RateLimit
}

// RateLimit is the provider's API quota after a request. Zero values mean
// the provider didn't report it.
type RateLimit struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// RateLimitError means the API quota is exhausted until Reset.
type RateLimitError struct {
	Reset time.Time
}

func (e *RateLimitError) Error() string {
	return "API rate limit exceeded until " + e.Reset.Format(time.RFC3339)
}

// ErrRefNotFound means the repo or ref does not exist (or isn't visible).
var ErrRefNotFound = errors.New("repo or ref not found")

// ArchiveFetcher downloads a repo's source at a ref.
type ArchiveFetcher interface {
	// Archive returns a gzipped tarball whose entries sit under one
	// top-level directory. The caller closes it.
	Archive(ctx context.Context, repo Repo, ref string) (io.ReadCloser, error)
}

// LooksLikePrerelease guesses from a tag name whether it is a pre-release,
// for tags that have no release metadata ("v2.0.0-rc1", "1.0b2").
func LooksLikePrerelease(tag string) bool {
	return prereleaseTag.MatchString(strings.ToLower(tag))
}

var prereleaseTag = regexp.MustCompile(`(?:^|[-._0-9])(?:alpha|beta|rc|pre|preview|dev|snapshot|nightly|a|b)\.?[0-9]*$`)

// RefLister lists an upstream repo's releases or tags for polling.
type RefLister interface {
	// ListReleases returns recent published releases. When etag matches,
	// it returns NotModified instead.
	ListReleases(ctx context.Context, repo Repo, etag string) (RefList, error)
	// ListTags returns the repo's tags, for projects that don't publish releases.
	ListTags(ctx context.Context, repo Repo) (RefList, error)
}

// Errors returned by WebhookParser.ParseWebhook. The webhook handler maps
// these to HTTP status codes, so implementations must wrap them.
var (
	// ErrUnauthenticated means the signature was missing or did not match.
	ErrUnauthenticated = errors.New("webhook signature missing or invalid")
	// ErrMalformed means the delivery was authentic but could not be parsed.
	ErrMalformed = errors.New("malformed webhook delivery")
)

// WebhookParser verifies and normalizes incoming webhook deliveries.
//
// Implementations must verify authenticity before parsing the body, because
// payloads are untrusted until verified.
type WebhookParser interface {
	// Name is the provider's identifier, used in the webhook URL
	// (/webhooks/{name}) and stored with every event.
	Name() string
	// ParseWebhook verifies the delivery and returns the normalized event.
	// Events Fork Keeper does not act on return Kind == KindIgnored and a nil error.
	ParseWebhook(header http.Header, body []byte) (Event, error)
}

// CompareVersions orders tag names like version numbers: a leading "v" is
// ignored and digit runs compare numerically, so "v1.10.0" > "v1.9.2".
// A pre-release suffix sorts before its release ("1.0.0-rc1" < "1.0.0").
func CompareVersions(a, b string) int {
	a, b = strings.TrimPrefix(strings.ToLower(a), "v"), strings.TrimPrefix(strings.ToLower(b), "v")
	for a != "" && b != "" {
		ca, ra := versionChunk(a)
		cb, rb := versionChunk(b)
		if c := compareChunk(ca, cb); c != 0 {
			return c
		}
		a, b = ra, rb
	}
	switch {
	case a == b:
		return 0
	case a == "":
		// "1.0" vs "1.0-rc1": the longer one is a pre-release if its
		// remainder starts with a separator and a letter.
		if isPrereleaseSuffix(b) {
			return 1
		}
		return -1
	default:
		if isPrereleaseSuffix(a) {
			return -1
		}
		return 1
	}
}

func versionChunk(s string) (chunk, rest string) {
	isDigit := s[0] >= '0' && s[0] <= '9'
	i := 1
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') == isDigit {
		i++
	}
	return s[:i], s[i:]
}

func compareChunk(a, b string) int {
	aNum, bNum := a[0] >= '0' && a[0] <= '9', b[0] >= '0' && b[0] <= '9'
	switch {
	case aNum && bNum:
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case aNum:
		return 1 // "1.0.1" > "1.0-rc"
	case bNum:
		return -1
	}
	return strings.Compare(a, b)
}

func isPrereleaseSuffix(s string) bool {
	s = strings.TrimLeft(s, "-._+")
	return s != "" && s[0] >= 'a' && s[0] <= 'z'
}

// GitRemote gives git access to repos over HTTPS.
type GitRemote interface {
	// CloneURL returns the HTTPS URL used to fetch from and push to repo.
	CloneURL(repo Repo) string
	// GitAuth returns the HTTP Authorization header value for git
	// operations on repo, or an empty Secret for anonymous access.
	GitAuth(ctx context.Context, repo Repo) (credentials.Secret, error)
}

// PullRequest is an open or closed pull (merge) request.
type PullRequest struct {
	Number  int
	URL     string
	State   string // "open" or "closed"
	Merged  bool
	Draft   bool
	HeadSHA string
	Labels  []string
}

// PullRequestInput is the content of a new or updated pull request.
type PullRequestInput struct {
	Title string
	Body  string
	// Head and Base are branch names in the same repo. Ignored on update.
	Head  string
	Base  string
	Draft bool
}

// CIState summarizes the checks on a commit.
type CIState string

const (
	// CINone means no checks or statuses were reported.
	CINone    CIState = "none"
	CIPending CIState = "pending"
	CISuccess CIState = "success"
	CIFailure CIState = "failure"
)

// CIStatus is the combined result of every check on a commit.
type CIStatus struct {
	State CIState
	// Failed names the checks that failed, for messages.
	Failed []string
	Total  int
}

// ForkHost is what the merge engine needs from the provider hosting a fork.
type ForkHost interface {
	GitRemote
	DefaultBranch(ctx context.Context, repo Repo) (string, error)
	// FindPullRequest returns the open PR from head into base, or nil.
	FindPullRequest(ctx context.Context, repo Repo, head, base string) (*PullRequest, error)
	CreatePullRequest(ctx context.Context, repo Repo, in PullRequestInput) (*PullRequest, error)
	// UpdatePullRequest changes the title and body of an open PR.
	UpdatePullRequest(ctx context.Context, repo Repo, number int, in PullRequestInput) (*PullRequest, error)
	GetPullRequest(ctx context.Context, repo Repo, number int) (*PullRequest, error)
	AddLabels(ctx context.Context, repo Repo, number int, labels []string) error
	CIStatus(ctx context.Context, repo Repo, sha string) (CIStatus, error)
	// MergePullRequest merges with a merge commit, only if head is still sha.
	MergePullRequest(ctx context.Context, repo Repo, number int, sha, title string) error
}

// BranchHead is the head commit of a repo's default branch.
type BranchHead struct {
	SHA string
	// ETag identifies this response for the next conditional request.
	ETag string
	// NotModified means the ETag matched and SHA is empty.
	NotModified bool
	RateLimit   RateLimit
}

// TreeFile is a file in a repo at some commit.
type TreeFile struct {
	Path string
	// ID is the host's content ID (the git blob SHA): equal IDs mean equal
	// content.
	ID   string
	Size int64
}

// BranchReader reads an upstream's default branch without cloning it, for
// the early warning.
type BranchReader interface {
	// DefaultHead returns the default branch's head commit, or NotModified
	// when etag still matches.
	DefaultHead(ctx context.Context, repo Repo, etag string) (BranchHead, error)
	// ListFiles lists the files directly in dir ("" for the root) at commit.
	// Subdirectories are listed with a trailing "/" and an empty ID.
	ListFiles(ctx context.Context, repo Repo, commit, dir string) ([]TreeFile, error)
	// ReadFile returns a file's content by its ID.
	ReadFile(ctx context.Context, repo Repo, id string) ([]byte, error)
}
