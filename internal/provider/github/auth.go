package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// ExpiryWarning is how far ahead Validate warns about an expiring credential.
const ExpiryWarning = 14 * 24 * time.Hour

// tokenRefreshMargin renews installation tokens this long before they expire.
const tokenRefreshMargin = 5 * time.Minute

// TokenSource issues tokens for API and git operations on a repo.
type TokenSource interface {
	Token(ctx context.Context, repo provider.Repo) (credentials.Secret, error)
	// Validate makes one GitHub call to confirm the credential works and
	// reports what it can do.
	Validate(ctx context.Context) (Report, error)
	// Reset drops cached tokens, for example after a credential reload.
	Reset()
}

// Report describes a validated credential.
type Report struct {
	Kind        string // "app" or "pat"
	Identity    string // App slug or user login
	Permissions map[string]string
	Events      []string
	Scopes      []string  // classic PATs only
	ExpiresAt   time.Time // zero if the credential does not expire
	Warnings    []string
}

// Minimum App permissions and webhook events (spec §6.4).
var (
	requiredPermissions = map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"metadata":      "read",
	}
	requiredEvents = []string{"create", "release"}
	permissionRank = map[string]int{"read": 1, "write": 2, "admin": 3}
)

// ---- GitHub App ----

// AppAuth authenticates as a GitHub App installation. The only long-lived
// secret is the App private key; installation tokens last about an hour and
// are cached until shortly before they expire.
type AppAuth struct {
	api            *apiClient
	creds          credentials.Provider
	appID          int64
	installationID int64
	keyName        string
	now            func() time.Time

	mu       sync.Mutex
	tokens   map[int64]installationToken
	installs map[string]int64 // repo full name -> installation ID
}

type installationToken struct {
	token     credentials.Secret
	expiresAt time.Time
}

// AppConfig configures an AppAuth.
type AppConfig struct {
	APIURL string
	AppID  int64
	// InstallationID, when 0, is looked up per repo.
	InstallationID int64
	// PrivateKey is the credential name of the PEM private key.
	PrivateKey string
	HTTPClient *http.Client
}

// NewAppAuth returns a TokenSource for a GitHub App.
func NewAppAuth(cfg AppConfig, creds credentials.Provider) *AppAuth {
	return &AppAuth{
		api:            newAPIClient(cfg.APIURL, cfg.HTTPClient),
		creds:          creds,
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		keyName:        cfg.PrivateKey,
		now:            time.Now,
	}
}

var _ TokenSource = (*AppAuth)(nil)

func (a *AppAuth) Reset() {
	a.mu.Lock()
	a.tokens = nil
	a.installs = nil
	a.mu.Unlock()
}

func (a *AppAuth) jwt(ctx context.Context) (string, error) {
	pemKey, err := a.creds.Get(ctx, a.keyName)
	if err != nil {
		return "", err
	}
	key, err := parsePrivateKey(pemKey.Bytes())
	if err != nil {
		return "", fmt.Errorf("credential %q: %w", a.keyName, err)
	}
	return appJWT(key, a.appID, a.now())
}

func (a *AppAuth) Token(ctx context.Context, repo provider.Repo) (credentials.Secret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	id, err := a.installationFor(ctx, repo)
	if err != nil {
		return credentials.Secret{}, err
	}
	if t, ok := a.tokens[id]; ok && a.now().Add(tokenRefreshMargin).Before(t.expiresAt) {
		return t.token, nil
	}

	jwt, err := a.jwt(ctx)
	if err != nil {
		return credentials.Secret{}, err
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := a.api.do(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", id), jwt, nil, &resp); err != nil {
		return credentials.Secret{}, fmt.Errorf("create installation token: %w", err)
	}
	if resp.Token == "" {
		return credentials.Secret{}, errors.New("create installation token: empty token in response")
	}
	t := installationToken{token: credentials.NewSecret([]byte(resp.Token)), expiresAt: resp.ExpiresAt}
	if a.tokens == nil {
		a.tokens = make(map[int64]installationToken)
	}
	a.tokens[id] = t
	return t.token, nil
}

// installationFor returns the configured installation ID, or looks up the
// one covering repo. Callers hold a.mu.
func (a *AppAuth) installationFor(ctx context.Context, repo provider.Repo) (int64, error) {
	if a.installationID != 0 {
		return a.installationID, nil
	}
	if id, ok := a.installs[repo.FullName()]; ok {
		return id, nil
	}
	jwt, err := a.jwt(ctx)
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/installation"
	if _, err := a.api.do(ctx, http.MethodGet, path, jwt, nil, &resp); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return 0, fmt.Errorf("GitHub App is not installed on %s", repo.FullName())
		}
		return 0, fmt.Errorf("find installation for %s: %w", repo.FullName(), err)
	}
	if a.installs == nil {
		a.installs = make(map[string]int64)
	}
	a.installs[repo.FullName()] = resp.ID
	return resp.ID, nil
}

func (a *AppAuth) Validate(ctx context.Context) (Report, error) {
	jwt, err := a.jwt(ctx)
	if err != nil {
		return Report{}, err
	}
	var app struct {
		Slug        string            `json:"slug"`
		Permissions map[string]string `json:"permissions"`
		Events      []string          `json:"events"`
	}
	if _, err := a.api.do(ctx, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		return Report{}, fmt.Errorf("validate GitHub App %d: %w", a.appID, err)
	}

	r := Report{Kind: "app", Identity: app.Slug, Permissions: app.Permissions, Events: app.Events}
	for _, perm := range slices.Sorted(maps.Keys(requiredPermissions)) {
		want := requiredPermissions[perm]
		if have := app.Permissions[perm]; permissionRank[have] < permissionRank[want] {
			if have == "" {
				have = "none"
			}
			r.Warnings = append(r.Warnings, fmt.Sprintf("App permission %q is %s; Fork Keeper needs %s", perm, have, want))
		}
	}
	for _, ev := range requiredEvents {
		if !slices.Contains(app.Events, ev) {
			r.Warnings = append(r.Warnings, fmt.Sprintf("App is not subscribed to the %q webhook event", ev))
		}
	}
	return r, nil
}

// ---- Personal access token ----

// PATAuth uses a fine-grained personal access token. A PAT can't receive
// webhooks on its own, so a separate webhook secret is still required.
type PATAuth struct {
	api       *apiClient
	creds     credentials.Provider
	tokenName string
	now       func() time.Time
}

// NewPATAuth returns a TokenSource that reads the token named tokenName.
func NewPATAuth(apiURL, tokenName string, creds credentials.Provider, hc *http.Client) *PATAuth {
	return &PATAuth{api: newAPIClient(apiURL, hc), creds: creds, tokenName: tokenName, now: time.Now}
}

var _ TokenSource = (*PATAuth)(nil)

func (p *PATAuth) Token(ctx context.Context, _ provider.Repo) (credentials.Secret, error) {
	return p.creds.Get(ctx, p.tokenName)
}

func (p *PATAuth) Reset() {}

func (p *PATAuth) Validate(ctx context.Context) (Report, error) {
	tok, err := p.creds.Get(ctx, p.tokenName)
	if err != nil {
		return Report{}, err
	}
	var user struct {
		Login string `json:"login"`
	}
	h, err := p.api.do(ctx, http.MethodGet, "/user", tok.Reveal(), nil, &user)
	if err != nil {
		return Report{}, fmt.Errorf("validate personal access token: %w", err)
	}

	r := Report{Kind: "pat", Identity: user.Login}
	// Classic tokens report scopes; fine-grained tokens don't send the header.
	if scopes := h.Get("X-OAuth-Scopes"); h.Values("X-OAuth-Scopes") != nil {
		for _, s := range strings.Split(scopes, ",") {
			if s = strings.TrimSpace(s); s != "" {
				r.Scopes = append(r.Scopes, s)
			}
		}
		r.Warnings = append(r.Warnings,
			"this is a classic token with account-wide access; use a fine-grained token limited to your forks")
	}
	if exp := h.Get("GitHub-Authentication-Token-Expiration"); exp != "" {
		if t, ok := parseExpiration(exp); ok {
			r.ExpiresAt = t
			if left := t.Sub(p.now()); left < ExpiryWarning {
				r.Warnings = append(r.Warnings, fmt.Sprintf("token expires %s (in %s)",
					t.Format(time.RFC3339), left.Round(time.Hour)))
			}
		}
	}
	return r, nil
}

func parseExpiration(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---- Per-repo routing ----

// Auth picks a TokenSource for each repo using a profile mapping, so
// different repos or owners can use different Apps or tokens.
type Auth struct {
	sources    map[string]TokenSource
	profileFor func(fullName string) string
}

// NewAuth returns a router. profileFor maps "owner/name" to a profile name,
// returning "" when no profile applies.
func NewAuth(sources map[string]TokenSource, profileFor func(fullName string) string) *Auth {
	return &Auth{sources: sources, profileFor: profileFor}
}

// Source returns the TokenSource for repo.
func (a *Auth) Source(repo provider.Repo) (TokenSource, string, error) {
	name := a.profileFor(repo.FullName())
	if name == "" {
		return nil, "", fmt.Errorf("no GitHub auth profile configured for %s", repo.FullName())
	}
	src, ok := a.sources[name]
	if !ok {
		return nil, "", fmt.Errorf("GitHub auth profile %q for %s does not exist", name, repo.FullName())
	}
	return src, name, nil
}

// Token returns a token for repo from its mapped profile.
func (a *Auth) Token(ctx context.Context, repo provider.Repo) (credentials.Secret, error) {
	src, _, err := a.Source(repo)
	if err != nil {
		return credentials.Secret{}, err
	}
	return src.Token(ctx, repo)
}

// Profiles returns the configured profile names, sorted.
func (a *Auth) Profiles() []string { return slices.Sorted(maps.Keys(a.sources)) }

// Profile returns the named TokenSource.
func (a *Auth) Profile(name string) TokenSource { return a.sources[name] }

// Reset drops cached tokens in every profile.
func (a *Auth) Reset() {
	for _, s := range a.sources {
		s.Reset()
	}
}
