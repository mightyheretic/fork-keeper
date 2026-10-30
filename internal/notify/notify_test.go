package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

func static(s string) SecretFunc {
	return func(context.Context) (credentials.Secret, error) { return credentials.NewSecret([]byte(s)), nil }
}

var testMsg = Message{Kind: KindLicense, Repo: "up/app", Ref: "v2", Title: "up/app v2 needs approval (#3)",
	Body: "MIT -> GPL-3.0", Priority: PriorityHigh, Tags: []string{"warning"}, Click: "https://github.com/me/app/pull/7",
	Links: []Link{{Label: "Open PR", URL: "https://github.com/me/app/pull/7"}}}

type captured struct {
	path   string
	header http.Header
	body   map[string]any
}

func capture(t *testing.T, status int) (*httptest.Server, *[]captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, captured{path: r.URL.Path, header: r.Header, body: body})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestNtfy(t *testing.T) {
	srv, got := capture(t, http.StatusOK)
	n := &Ntfy{URL: srv.URL + "/fk-alerts", Token: static("tk_secret"), HTTP: srv.Client()}
	if err := n.Send(context.Background(), testMsg); err != nil {
		t.Fatal(err)
	}
	c := (*got)[0]
	if c.path != "/" || c.body["topic"] != "fk-alerts" || c.body["title"] != testMsg.Title || c.body["priority"] != 4.0 ||
		c.header.Get("Authorization") != "Bearer tk_secret" || c.body["click"] != testMsg.Click {
		t.Fatalf("request = %+v", c)
	}
	actions := c.body["actions"].([]any)
	if a := actions[0].(map[string]any); a["action"] != "view" || a["url"] != testMsg.Links[0].URL {
		t.Fatalf("actions = %v", actions)
	}
}

func TestGotifyAndWebhook(t *testing.T) {
	srv, got := capture(t, http.StatusOK)
	g := &Gotify{URL: srv.URL + "/", Token: static("app-token"), HTTP: srv.Client()}
	if err := g.Send(context.Background(), testMsg); err != nil {
		t.Fatal(err)
	}
	c := (*got)[0]
	if c.path != "/message" || c.header.Get("X-Gotify-Key") != "app-token" || c.body["priority"] != 8.0 ||
		!strings.Contains(c.body["message"].(string), "Open PR: https://github.com/me/app/pull/7") {
		t.Fatalf("gotify request = %+v", c)
	}

	w := &Webhook{URL: srv.URL + "/hook", HTTP: srv.Client()}
	if err := w.Send(context.Background(), testMsg); err != nil {
		t.Fatal(err)
	}
	c = (*got)[1]
	if c.path != "/hook" || c.header.Get("Authorization") != "" || c.body["repo"] != "up/app" || c.body["kind"] != "license" {
		t.Fatalf("webhook request = %+v", c)
	}
}

func TestHTTPErrorIsReported(t *testing.T) {
	srv, _ := capture(t, http.StatusForbidden)
	err := (&Webhook{URL: srv.URL, HTTP: srv.Client()}).Send(context.Background(), testMsg)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

// smtpServer is a minimal SMTP server. With startTLS it offers STARTTLS
// and AUTH only after it.
type smtpServer struct {
	addr    string
	tls     *tls.Config
	mu      sync.Mutex
	auth    string
	data    string
	rcpts   []string
	usedTLS bool
}

func newSMTPServer(t *testing.T, startTLS bool) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &smtpServer{addr: ln.Addr().String()}
	if startTLS {
		s.tls = selfSigned(t)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *smtpServer) serve(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), conn
	io.WriteString(w, "220 test ESMTP\r\n")
	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		switch {
		case cmd == "EHLO":
			switch {
			case s.tls != nil && !secure:
				io.WriteString(w, "250-test\r\n250 STARTTLS\r\n")
			default:
				io.WriteString(w, "250-test\r\n250 AUTH PLAIN\r\n")
			}
		case cmd == "STARTTLS":
			io.WriteString(w, "220 go ahead\r\n")
			tc := tls.Server(conn, s.tls)
			if tc.Handshake() != nil {
				return
			}
			conn, secure = tc, true
			r, w = bufio.NewReader(tc), tc
			s.mu.Lock()
			s.usedTLS = true
			s.mu.Unlock()
		case cmd == "AUTH":
			s.mu.Lock()
			s.auth = strings.TrimSpace(strings.Fields(line)[2])
			s.mu.Unlock()
			io.WriteString(w, "235 ok\r\n")
		case cmd == "RCPT":
			s.mu.Lock()
			s.rcpts = append(s.rcpts, strings.TrimSpace(line))
			s.mu.Unlock()
			io.WriteString(w, "250 ok\r\n")
		case cmd == "DATA":
			io.WriteString(w, "354 go\r\n")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			io.WriteString(w, "250 queued\r\n")
		case cmd == "QUIT":
			io.WriteString(w, "221 bye\r\n")
			return
		default:
			io.WriteString(w, "250 ok\r\n")
		}
	}
}

func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func TestEmailUsesSTARTTLS(t *testing.T) {
	srv := newSMTPServer(t, true)
	e := &Email{Addr: srv.addr, Username: "me", Password: static("pw"), From: "fk@example.com",
		To: []string{"me@example.com"}, TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	m := testMsg
	m.Title = "evil\r\nBcc: victim@example.com"
	m.Body = "line one\n.\nline three"
	if err := e.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.usedTLS {
		t.Fatal("not encrypted")
	}
	if dec, _ := base64.StdEncoding.DecodeString(srv.auth); string(dec) != "\x00me\x00pw" {
		t.Fatalf("auth = %q", dec)
	}
	if len(srv.rcpts) != 1 || !strings.Contains(srv.rcpts[0], "me@example.com") {
		t.Fatalf("rcpts = %v", srv.rcpts)
	}
	if strings.Contains(srv.data, "\r\nBcc:") || !strings.Contains(srv.data, "Subject: [Fork Keeper] evil  Bcc:") {
		t.Fatalf("header injection:\n%s", srv.data)
	}
	if !strings.Contains(srv.data, "line one\r\n..\r\nline three") || !strings.Contains(srv.data, "Open PR: https://github.com/me/app/pull/7") {
		t.Fatalf("body:\n%s", srv.data)
	}
}

func TestEmailRefusesPlaintext(t *testing.T) {
	srv := newSMTPServer(t, false)
	e := &Email{Addr: srv.addr, From: "fk@example.com", To: []string{"me@example.com"}}
	if err := e.Send(context.Background(), testMsg); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v", err)
	}
}

// fakeChannel fails until ok is set.
type fakeChannel struct {
	name string
	ok   bool
	sent []Message
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Send(_ context.Context, m Message) error {
	if !f.ok {
		return errors.New("down")
	}
	f.sent = append(f.sent, m)
	return nil
}

func TestSenderRetriesOnlyFailedChannels(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Notifications{Ntfy: &config.Ntfy{URL: "https://ntfy.sh/x"}}
	q := NewQueue(st, cfg)
	good, bad := &fakeChannel{name: "ntfy", ok: true}, &fakeChannel{name: "email"}
	s := NewSender(st, q, []Channel{good, bad}, cfg, slog.New(slog.DiscardHandler))

	if ok, err := q.Notify(ctx, Notice{Key: "k1", Message: testMsg}); !ok || err != nil {
		t.Fatalf("queue: %v %v", ok, err)
	}
	// Send "two minutes ago", so the one-minute retry is already due.
	s.now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(good.sent) != 1 {
		t.Fatalf("good channel got %d", len(good.sent))
	}
	list, _ := st.Notifications(ctx, 1)
	if list[0].Status != store.NotifyPending || !strings.Contains(list[0].LastError, "email: down") {
		t.Fatalf("after failure: %+v", list[0])
	}
	// The retry only goes to the failed channel.
	bad.ok = true
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(good.sent) != 1 || len(bad.sent) != 1 {
		t.Fatalf("sent good=%d bad=%d", len(good.sent), len(bad.sent))
	}
	if list, _ = st.Notifications(ctx, 1); list[0].Status != store.NotifySent {
		t.Fatalf("after retry: %+v", list[0])
	}
}

func TestQueueRespectsSettings(t *testing.T) {
	off := false
	zero := time.Duration(0)
	q := NewQueue(nil, config.Notifications{Ntfy: &config.Ntfy{}, Approvals: &off, RemindAfter: &zero})
	if q.Wants(KindApproval) || q.Wants(KindReminder) || !q.Wants(KindLicense) || !q.Wants(KindSync) {
		t.Fatal("wrong kinds wanted")
	}
	if q.Wants(KindPR) || !NewQueue(nil, config.Notifications{Ntfy: &config.Ntfy{}, PullRequests: true}).Wants(KindPR) {
		t.Fatal("PR notices are opt-in")
	}
	if NewQueue(nil, config.Notifications{}).Wants(KindLicense) {
		t.Fatal("no channels, but notices are queued")
	}
}

func TestReminder(t *testing.T) {
	a := store.Approval{ID: 3, RefKey: store.RefKey{Repo: "up/app", Ref: "v2"}, Kind: store.ApprovalOverride,
		CreatedAt: time.Now().Add(-73 * time.Hour), Context: json.RawMessage(`{"rule":"r","reason":"MIT -> GPL-3.0"}`)}
	m := Reminder(a, "https://github.com/me/app/pull/7", time.Now())
	if !strings.Contains(m.Body, "3 days") || !strings.Contains(m.Body, `approve --reason "why" 3`) ||
		!strings.Contains(m.Body, LabelApproved) || m.Click == "" {
		t.Fatalf("reminder = %+v", m)
	}
}
