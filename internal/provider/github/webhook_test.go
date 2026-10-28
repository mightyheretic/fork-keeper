package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

const testSecret = "s3cret"

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func headers(event, delivery, sig string) http.Header {
	h := http.Header{}
	if event != "" {
		h.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		h.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		h.Set("X-Hub-Signature-256", sig)
	}
	return h
}

func newTestWebhooks() *Webhooks {
	return NewWebhooks(func() ([]byte, error) { return []byte(testSecret), nil })
}

const releaseBody = `{
	"action": "published",
	"release": {"tag_name": "v2.0.0", "name": "Two", "draft": false, "prerelease": true},
	"repository": {"name": "app", "clone_url": "https://github.com/up/app.git", "owner": {"login": "up"}}
}`

func TestVerify(t *testing.T) {
	body := []byte(releaseBody)
	tests := []struct {
		name string
		sig  string
	}{
		{"missing", ""},
		{"wrong secret", sign("other", body)},
		{"sha1 prefix", "sha1=" + sign(testSecret, body)[len("sha256="):]},
		{"not hex", "sha256=zzzz"},
		{"truncated", sign(testSecret, body)[:20]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTestWebhooks().ParseWebhook(headers("release", "d1", tt.sig), body)
			if !errors.Is(err, provider.ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestVerifyTamperedBody(t *testing.T) {
	sig := sign(testSecret, []byte(releaseBody))
	tampered := []byte(releaseBody + " ")
	_, err := newTestWebhooks().ParseWebhook(headers("release", "d1", sig), tampered)
	if !errors.Is(err, provider.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestSecretErrorsFailClosed(t *testing.T) {
	body := []byte(releaseBody)
	for name, fn := range map[string]SecretFunc{
		"load error": func() ([]byte, error) { return nil, errors.New("boom") },
		"empty":      func() ([]byte, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			// An empty secret must not verify a signature computed with an empty key.
			_, err := NewWebhooks(fn).ParseWebhook(headers("release", "d1", sign("", body)), body)
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, provider.ErrUnauthenticated) || errors.Is(err, provider.ErrMalformed) {
				t.Fatalf("err = %v, want an internal error", err)
			}
		})
	}
}

func TestParseRelease(t *testing.T) {
	body := []byte(releaseBody)
	ev, err := newTestWebhooks().ParseWebhook(headers("release", "d1", sign(testSecret, body)), body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != provider.KindRelease || ev.Tag != "v2.0.0" || ev.ReleaseName != "Two" || !ev.Prerelease {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Repo.FullName() != "up/app" || ev.Repo.CloneURL != "https://github.com/up/app.git" {
		t.Fatalf("unexpected repo: %+v", ev.Repo)
	}
	if ev.Provider != "github" || ev.DeliveryID != "d1" || ev.ReceivedAt.IsZero() {
		t.Fatalf("unexpected metadata: %+v", ev)
	}
}

func TestParseKinds(t *testing.T) {
	tests := []struct {
		name  string
		event string
		body  string
		want  provider.EventKind
		tag   string
	}{
		{"ping", "ping", `{"zen":"hi"}`, provider.KindPing, ""},
		{"release edited", "release", `{"action":"edited","release":{"tag_name":"v1"},"repository":{"name":"a","owner":{"login":"o"}}}`, provider.KindIgnored, ""},
		{"draft release", "release", `{"action":"published","release":{"tag_name":"v1","draft":true},"repository":{"name":"a","owner":{"login":"o"}}}`, provider.KindIgnored, ""},
		{"tag created", "create", `{"ref":"v1.2.3","ref_type":"tag","repository":{"name":"a","owner":{"login":"o"}}}`, provider.KindTag, "v1.2.3"},
		{"branch created", "create", `{"ref":"feature","ref_type":"branch","repository":{"name":"a","owner":{"login":"o"}}}`, provider.KindIgnored, ""},
		{"push", "push", `{}`, provider.KindIgnored, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			ev, err := newTestWebhooks().ParseWebhook(headers(tt.event, "d1", sign(testSecret, body)), body)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != tt.want || ev.Tag != tt.tag {
				t.Fatalf("got kind=%q tag=%q, want kind=%q tag=%q", ev.Kind, ev.Tag, tt.want, tt.tag)
			}
		})
	}
}

func TestParseMalformed(t *testing.T) {
	tests := []struct {
		name, event, delivery, body string
	}{
		{"missing delivery", "release", "", releaseBody},
		{"missing event", "", "d1", releaseBody},
		{"bad json", "release", "d1", `{"action":`},
		{"release without tag", "release", "d1", `{"action":"published","release":{},"repository":{"name":"a","owner":{"login":"o"}}}`},
		{"release without repo", "release", "d1", `{"action":"published","release":{"tag_name":"v1"}}`},
		{"tag without ref", "create", "d1", `{"ref_type":"tag","repository":{"name":"a","owner":{"login":"o"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			_, err := newTestWebhooks().ParseWebhook(headers(tt.event, tt.delivery, sign(testSecret, body)), body)
			if !errors.Is(err, provider.ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
}
