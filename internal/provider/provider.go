// Package provider defines the provider-neutral types that the Fork Keeper core
// works with. Each git host (GitHub, GitLab, Forgejo, ...) lives in its own
// subpackage and implements these interfaces; nothing outside those
// subpackages may import host-specific code.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
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
