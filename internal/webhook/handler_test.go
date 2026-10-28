package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

const secret = "s3cret"

const releaseBody = `{"action":"published","release":{"tag_name":"v1.0.0"},"repository":{"name":"app","owner":{"login":"up"}}}`

type fixture struct {
	srv   *httptest.Server
	store *store.Store
	wakes int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	f := &fixture{store: s}
	mux := http.NewServeMux()
	NewHandler(s, func() { f.wakes++ }, slog.New(slog.DiscardHandler),
		github.NewWebhooks(func() ([]byte, error) { return []byte(secret), nil }),
	).Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) post(t *testing.T, path, event, delivery, body, sig string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestQueuesAndDedupes(t *testing.T) {
	f := newFixture(t)

	code, msg := f.post(t, "/webhooks/github", "release", "d1", releaseBody, sign(releaseBody))
	if code != http.StatusAccepted || msg != "queued" {
		t.Fatalf("first delivery: %d %q", code, msg)
	}
	code, msg = f.post(t, "/webhooks/github", "release", "d1", releaseBody, sign(releaseBody))
	if code != http.StatusOK || msg != "duplicate" {
		t.Fatalf("redelivery: %d %q", code, msg)
	}
	if f.wakes != 1 {
		t.Fatalf("worker woken %d times, want 1", f.wakes)
	}

	status, _, err := f.store.DeliveryStatus(context.Background(), "github", "d1")
	if err != nil || status != store.StatusPending {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestResponses(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, path, event, body, sig string
		want                         int
	}{
		{"bad signature", "/webhooks/github", "release", releaseBody, sign("other"), http.StatusUnauthorized},
		{"unsigned", "/webhooks/github", "release", releaseBody, "", http.StatusUnauthorized},
		{"unknown provider", "/webhooks/gitlab", "release", releaseBody, sign(releaseBody), http.StatusNotFound},
		{"malformed", "/webhooks/github", "release", `{"action":`, sign(`{"action":`), http.StatusBadRequest},
		{"ping", "/webhooks/github", "ping", `{}`, sign(`{}`), http.StatusOK},
		{"ignored", "/webhooks/github", "push", `{}`, sign(`{}`), http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, msg := f.post(t, tt.path, tt.event, "d-"+tt.name, tt.body, tt.sig)
			if code != tt.want {
				t.Fatalf("status = %d (%q), want %d", code, msg, tt.want)
			}
		})
	}
	if f.wakes != 0 {
		t.Fatalf("worker woken %d times for non-queued deliveries", f.wakes)
	}
}

func TestRejectsOversizedBody(t *testing.T) {
	f := newFixture(t)
	body := strings.Repeat("x", MaxBodyBytes+1)
	code, _ := f.post(t, "/webhooks/github", "release", "big", body, sign(body))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", code)
	}
}

func TestGetNotAllowed(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/webhooks/github")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
