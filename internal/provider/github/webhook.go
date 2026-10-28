// Package github implements the provider interfaces for GitHub.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// Name is the provider identifier used in /webhooks/github.
const Name = "github"

const (
	headerSignature = "X-Hub-Signature-256"
	headerEvent     = "X-GitHub-Event"
	headerDelivery  = "X-GitHub-Delivery"
	signaturePrefix = "sha256="
)

// SecretFunc returns the current webhook signing secret. It is called on
// every delivery so that a rotated secret takes effect without a restart.
type SecretFunc func() ([]byte, error)

// Webhooks verifies and normalizes GitHub webhook deliveries.
type Webhooks struct {
	secret SecretFunc
	now    func() time.Time
}

// NewWebhooks returns a parser that verifies deliveries against secret.
func NewWebhooks(secret SecretFunc) *Webhooks {
	return &Webhooks{secret: secret, now: time.Now}
}

var _ provider.WebhookParser = (*Webhooks)(nil)

func (w *Webhooks) Name() string { return Name }

func (w *Webhooks) ParseWebhook(header http.Header, body []byte) (provider.Event, error) {
	if err := w.verify(header.Get(headerSignature), body); err != nil {
		return provider.Event{}, err
	}

	delivery := header.Get(headerDelivery)
	if delivery == "" {
		return provider.Event{}, fmt.Errorf("%w: missing %s header", provider.ErrMalformed, headerDelivery)
	}
	ev := provider.Event{
		Provider:   Name,
		DeliveryID: delivery,
		Kind:       provider.KindIgnored,
		ReceivedAt: w.now().UTC(),
	}

	switch name := header.Get(headerEvent); name {
	case "ping":
		ev.Kind = provider.KindPing
		return ev, nil
	case "release":
		return parseRelease(ev, body)
	case "create":
		return parseCreate(ev, body)
	case "":
		return provider.Event{}, fmt.Errorf("%w: missing %s header", provider.ErrMalformed, headerEvent)
	default:
		return ev, nil
	}
}

// verify checks the HMAC-SHA256 signature of body in constant time.
func (w *Webhooks) verify(sigHeader string, body []byte) error {
	hexSig, ok := strings.CutPrefix(sigHeader, signaturePrefix)
	if !ok {
		return provider.ErrUnauthenticated
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return provider.ErrUnauthenticated
	}
	secret, err := w.secret()
	if err != nil {
		// Fail closed, and don't include the secret source in the error.
		return fmt.Errorf("load webhook secret: %w", err)
	}
	if len(secret) == 0 {
		return fmt.Errorf("load webhook secret: secret is empty")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return provider.ErrUnauthenticated
	}
	return nil
}

type repository struct {
	Name     string `json:"name"`
	CloneURL string `json:"clone_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repository) toRepo() (provider.Repo, error) {
	if r.Owner.Login == "" || r.Name == "" {
		return provider.Repo{}, fmt.Errorf("%w: repository owner or name missing", provider.ErrMalformed)
	}
	return provider.Repo{Owner: r.Owner.Login, Name: r.Name, CloneURL: r.CloneURL}, nil
}

func parseRelease(ev provider.Event, body []byte) (provider.Event, error) {
	var p struct {
		Action  string `json:"action"`
		Release struct {
			TagName    string `json:"tag_name"`
			Name       string `json:"name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		} `json:"release"`
		Repository repository `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("%w: %v", provider.ErrMalformed, err)
	}
	// "published" fires once for every release and pre-release. Other actions
	// ("created", "released", "edited", ...) would duplicate or don't apply.
	if p.Action != "published" || p.Release.Draft {
		return ev, nil
	}
	repo, err := p.Repository.toRepo()
	if err != nil {
		return provider.Event{}, err
	}
	if p.Release.TagName == "" {
		return provider.Event{}, fmt.Errorf("%w: release has no tag_name", provider.ErrMalformed)
	}
	ev.Kind = provider.KindRelease
	ev.Repo = repo
	ev.Tag = p.Release.TagName
	ev.ReleaseName = p.Release.Name
	ev.Prerelease = p.Release.Prerelease
	return ev, nil
}

func parseCreate(ev provider.Event, body []byte) (provider.Event, error) {
	var p struct {
		Ref        string     `json:"ref"`
		RefType    string     `json:"ref_type"`
		Repository repository `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return provider.Event{}, fmt.Errorf("%w: %v", provider.ErrMalformed, err)
	}
	if p.RefType != "tag" {
		return ev, nil
	}
	repo, err := p.Repository.toRepo()
	if err != nil {
		return provider.Event{}, err
	}
	if p.Ref == "" {
		return provider.Event{}, fmt.Errorf("%w: tag event has no ref", provider.ErrMalformed)
	}
	ev.Kind = provider.KindTag
	ev.Repo = repo
	ev.Tag = p.Ref
	return ev, nil
}
