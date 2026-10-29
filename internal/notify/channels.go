package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// maxBody keeps messages under ntfy's 4096-byte limit, with room for the
// title and JSON.
const maxBody = 3500

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // don't split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "\n…"
}

func postJSON(ctx context.Context, hc *http.Client, target string, body any, header http.Header) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fork-keeper")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The URL may carry an ntfy topic, which acts as a password.
		if uerr, ok := err.(*url.Error); ok {
			return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

func bearer(ctx context.Context, tok SecretFunc) (http.Header, error) {
	t, err := tok.get(ctx)
	if err != nil || t == "" {
		return nil, err
	}
	return http.Header{"Authorization": {"Bearer " + t}}, nil
}

// Ntfy publishes to an ntfy topic.
type Ntfy struct {
	// URL is the topic URL.
	URL   string
	Token SecretFunc
	HTTP  *http.Client
}

func (n *Ntfy) Name() string { return "ntfy" }

var ntfyPriority = map[Priority]int{PriorityLow: 2, PriorityDefault: 3, PriorityHigh: 4, PriorityUrgent: 5}

func (n *Ntfy) Send(ctx context.Context, m Message) error {
	u, err := url.Parse(n.URL)
	if err != nil {
		return fmt.Errorf("bad ntfy URL")
	}
	topic := strings.Trim(u.Path, "/")
	u.Path = "/"
	type action struct {
		Action string `json:"action"`
		Label  string `json:"label"`
		URL    string `json:"url"`
	}
	var actions []action
	for _, l := range m.Links {
		if len(actions) == 3 { // ntfy's limit
			break
		}
		actions = append(actions, action{Action: "view", Label: l.Label, URL: l.URL})
	}
	h, err := bearer(ctx, n.Token)
	if err != nil {
		return err
	}
	return postJSON(ctx, n.HTTP, u.String(), map[string]any{
		"topic": topic, "title": m.Title, "message": truncate(m.Body, maxBody), "priority": ntfyPriority[m.Priority],
		"tags": m.Tags, "click": m.Click, "actions": actions,
	}, h)
}

// Gotify sends to a Gotify server.
type Gotify struct {
	URL   string
	Token SecretFunc
	HTTP  *http.Client
}

func (g *Gotify) Name() string { return "gotify" }

var gotifyPriority = map[Priority]int{PriorityLow: 2, PriorityDefault: 5, PriorityHigh: 8, PriorityUrgent: 10}

func (g *Gotify) Send(ctx context.Context, m Message) error {
	tok, err := g.Token.get(ctx)
	if err != nil {
		return err
	}
	body := m.Body
	for _, l := range m.Links {
		body += fmt.Sprintf("\n%s: %s", l.Label, l.URL)
	}
	extras := map[string]any{}
	if m.Click != "" {
		extras["client::notification"] = map[string]any{"click": map[string]string{"url": m.Click}}
	}
	return postJSON(ctx, g.HTTP, strings.TrimRight(g.URL, "/")+"/message", map[string]any{
		"title": m.Title, "message": truncate(body, 8000), "priority": gotifyPriority[m.Priority], "extras": extras,
	}, http.Header{"X-Gotify-Key": {tok}})
}

// Webhook POSTs the message as JSON.
type Webhook struct {
	URL   string
	Token SecretFunc
	HTTP  *http.Client
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, m Message) error {
	h, err := bearer(ctx, w.Token)
	if err != nil {
		return err
	}
	return postJSON(ctx, w.HTTP, w.URL, m, h)
}

// Email sends mail over SMTP. The connection is always encrypted: implicit
// TLS on port 465, STARTTLS (required) otherwise.
type Email struct {
	Addr     string
	Username string
	Password SecretFunc
	From     string
	To       []string
	// TLSConfig overrides the TLS settings (tests).
	TLSConfig *tls.Config
}

func (e *Email) Name() string { return "email" }

func (e *Email) Send(ctx context.Context, m Message) error {
	host, port, err := net.SplitHostPort(e.Addr)
	if err != nil {
		return err
	}
	tlsCfg := e.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
	d := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	if port == "465" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", e.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", e.Addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS; refusing to send mail unencrypted", e.Addr)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if e.Username != "" {
		pw, err := e.Password.get(ctx)
		if err != nil {
			return err
		}
		if err := c.Auth(smtp.PlainAuth("", e.Username, pw, host)); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	if err := c.Mail(e.From); err != nil {
		return err
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("recipient %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(e.compose(m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// compose builds the RFC 5322 message. Header values come from upstream
// data (repo and tag names), so line breaks are stripped and non-ASCII is
// encoded.
func (e *Email) compose(m Message) []byte {
	clean := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	var id [12]byte
	rand.Read(id[:])
	domain := "forkkeeper.invalid"
	if at := strings.LastIndex(e.From, "@"); at >= 0 {
		domain = strings.Trim(e.From[at+1:], "> ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", clean(e.From))
	fmt.Fprintf(&b, "To: %s\r\n", clean(strings.Join(e.To, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", clean("[Fork Keeper] "+m.Title)))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id[:]), clean(domain))
	if m.Priority == PriorityUrgent || m.Priority == PriorityHigh {
		b.WriteString("X-Priority: 1\r\nImportance: high\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	body := m.Body
	if len(m.Links) > 0 {
		body += "\n"
		for _, l := range m.Links {
			body += fmt.Sprintf("\n%s: %s", l.Label, l.URL)
		}
	}
	// SMTP wants CRLF line endings, and a line with only "." ends DATA
	// (the dot-stuffing writer handles that).
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
