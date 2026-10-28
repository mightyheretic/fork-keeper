package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
)

const testAppID = 4242

// fakeGitHub is a minimal GitHub API that checks App JWTs.
type fakeGitHub struct {
	t           *testing.T
	pub         *rsa.PublicKey
	srv         *httptest.Server
	tokenMints  atomic.Int32
	lookups     atomic.Int32
	permissions map[string]string
	events      []string
	patHeaders  map[string]string
	tokenTTL    time.Duration
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey) *fakeGitHub {
	f := &fakeGitHub{
		t:           t,
		pub:         pub,
		permissions: map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"},
		events:      []string{"create", "release"},
		tokenTTL:    time.Hour,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"slug": "fork-keeper-test", "permissions": f.permissions, "events": f.events})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(w, r) {
			return
		}
		f.lookups.Add(1)
		if r.PathValue("owner") != "up" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 42})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !f.checkJWT(w, r) {
			return
		}
		n := f.tokenMints.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + r.PathValue("id") + "_" + string(rune('0'+n)),
			"expires_at": time.Now().Add(f.tokenTTL).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer github_pat_test" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		for k, v := range f.patHeaders {
			w.Header().Set(k, v)
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "mightyheretic"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// checkJWT verifies the RS256 signature and claims GitHub would check.
func (f *fakeGitHub) checkJWT(w http.ResponseWriter, r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	fail := func(msg string) bool {
		f.t.Errorf("bad JWT on %s: %s", r.URL.Path, msg)
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
		return false
	}
	if !ok || len(parts) != 3 {
		return fail("not a JWT")
	}
	enc := base64.RawURLEncoding
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig); err != nil {
		return fail("signature: " + err.Error())
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	b, _ := enc.DecodeString(parts[1])
	json.Unmarshal(b, &claims)
	now := time.Now().Unix()
	if claims.Iss != "4242" || claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600 {
		return fail("claims: " + string(b))
	}
	return true
}

func setupApp(t *testing.T, installationID int64) (*AppAuth, *fakeGitHub) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	os.WriteFile(filepath.Join(dir, "app_key"), pemBytes, 0o600)

	f := newFakeGitHub(t, &key.PublicKey)
	a := NewAppAuth(AppConfig{APIURL: f.srv.URL, AppID: testAppID, InstallationID: installationID, PrivateKey: "app_key"},
		credentials.File{Dir: dir})
	return a, f
}

var upApp = provider.Repo{Owner: "up", Name: "app"}

func TestAppTokenCachedAndRefreshed(t *testing.T) {
	a, f := setupApp(t, 0)
	ctx := context.Background()

	tok1, err := a.Token(ctx, upApp)
	if err != nil {
		t.Fatal(err)
	}
	tok2, _ := a.Token(ctx, upApp)
	if tok1.Reveal() != tok2.Reveal() || f.tokenMints.Load() != 1 || f.lookups.Load() != 1 {
		t.Fatalf("expected one mint and one lookup, got %d and %d", f.tokenMints.Load(), f.lookups.Load())
	}
	if !strings.HasPrefix(tok1.Reveal(), "ghs_42_") {
		t.Fatalf("token from wrong installation: %s", tok1.Reveal())
	}

	// A token inside the refresh margin is replaced on the next call.
	a.Reset()
	f.tokenTTL = 4 * time.Minute
	short, _ := a.Token(ctx, upApp)
	tok3, err := a.Token(ctx, upApp)
	if err != nil {
		t.Fatal(err)
	}
	if tok3.Reveal() == short.Reveal() || f.tokenMints.Load() != 3 {
		t.Fatalf("token was not refreshed before expiry (mints=%d)", f.tokenMints.Load())
	}

	a.Reset()
	a.Token(ctx, upApp)
	if f.tokenMints.Load() != 4 || f.lookups.Load() != 3 {
		t.Fatal("Reset did not drop cached tokens and installations")
	}
}

func TestAppConfiguredInstallationSkipsLookup(t *testing.T) {
	a, f := setupApp(t, 99)
	tok, err := a.Token(context.Background(), provider.Repo{Owner: "anyone", Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Reveal(), "ghs_99_") || f.lookups.Load() != 0 {
		t.Fatalf("token=%s lookups=%d", tok.Reveal(), f.lookups.Load())
	}
}

func TestAppNotInstalled(t *testing.T) {
	a, _ := setupApp(t, 0)
	_, err := a.Token(context.Background(), provider.Repo{Owner: "stranger", Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "not installed on stranger/x") {
		t.Fatalf("err = %v", err)
	}
}

func TestAppValidate(t *testing.T) {
	a, f := setupApp(t, 0)
	r, err := a.Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Identity != "fork-keeper-test" || len(r.Warnings) != 0 {
		t.Fatalf("report = %+v", r)
	}

	f.permissions = map[string]string{"contents": "read", "metadata": "read"}
	f.events = []string{"release"}
	r, _ = a.Validate(context.Background())
	got := strings.Join(r.Warnings, "\n")
	for _, want := range []string{`"contents" is read`, `"pull_requests" is none`, `"create" webhook event`} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings missing %q:\n%s", want, got)
		}
	}
}

func TestAppBadKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app_key"), []byte("not a pem"), 0o600)
	a := NewAppAuth(AppConfig{APIURL: "http://127.0.0.1:1", AppID: 1, PrivateKey: "app_key"}, credentials.File{Dir: dir})
	if _, err := a.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "not PEM") {
		t.Fatalf("err = %v", err)
	}
}

func setupPAT(t *testing.T, token string) (*PATAuth, *fakeGitHub) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pat"), []byte(token), 0o600)
	f := newFakeGitHub(t, nil)
	return NewPATAuth(f.srv.URL, "pat", credentials.File{Dir: dir}, nil), f
}

func TestPATValidate(t *testing.T) {
	p, f := setupPAT(t, "github_pat_test")
	ctx := context.Background()

	f.patHeaders = map[string]string{"GitHub-Authentication-Token-Expiration": time.Now().Add(90 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 MST")}
	r, err := p.Validate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Identity != "mightyheretic" || r.ExpiresAt.IsZero() || len(r.Warnings) != 0 {
		t.Fatalf("fine-grained report = %+v", r)
	}

	f.patHeaders = map[string]string{"GitHub-Authentication-Token-Expiration": time.Now().Add(3 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 -0700")}
	r, _ = p.Validate(ctx)
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "expires") {
		t.Fatalf("expiry warning missing: %+v", r.Warnings)
	}

	f.patHeaders = map[string]string{"X-OAuth-Scopes": "repo, workflow"}
	r, _ = p.Validate(ctx)
	if len(r.Scopes) != 2 || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "classic") {
		t.Fatalf("classic token report = %+v", r)
	}
}

func TestPATBadCredentials(t *testing.T) {
	p, _ := setupPAT(t, "github_pat_wrong")
	_, err := p.Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401 Bad credentials") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "github_pat_wrong") {
		t.Fatalf("error leaked the token: %v", err)
	}
}

type staticSource struct{ tok string }

func (s staticSource) Token(context.Context, provider.Repo) (credentials.Secret, error) {
	return credentials.NewSecret([]byte(s.tok)), nil
}
func (staticSource) Validate(context.Context) (Report, error) { return Report{}, nil }
func (staticSource) Reset()                                   {}

func TestAuthRouting(t *testing.T) {
	a := NewAuth(map[string]TokenSource{"work": staticSource{"w"}, "default": staticSource{"d"}},
		func(name string) string {
			switch {
			case strings.HasPrefix(name, "acme/"):
				return "work"
			case strings.HasPrefix(name, "ghost/"):
				return "missing"
			case strings.HasPrefix(name, "none/"):
				return ""
			}
			return "default"
		})
	ctx := context.Background()

	if tok, _ := a.Token(ctx, provider.Repo{Owner: "acme", Name: "x"}); tok.Reveal() != "w" {
		t.Errorf("acme/x got %q", tok.Reveal())
	}
	if tok, _ := a.Token(ctx, provider.Repo{Owner: "me", Name: "x"}); tok.Reveal() != "d" {
		t.Errorf("me/x got %q", tok.Reveal())
	}
	if _, err := a.Token(ctx, provider.Repo{Owner: "ghost", Name: "x"}); err == nil {
		t.Error("missing profile should error")
	}
	if _, err := a.Token(ctx, provider.Repo{Owner: "none", Name: "x"}); err == nil {
		t.Error("unmapped repo should error")
	}
}
