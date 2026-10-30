package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

// Notifications configures where Fork Keeper sends messages. Each channel
// is optional; every configured channel gets every message.
type Notifications struct {
	Ntfy    *Ntfy    `yaml:"ntfy"`
	Gotify  *Gotify  `yaml:"gotify"`
	Email   *Email   `yaml:"email"`
	Webhook *Webhook `yaml:"webhook"`
	// Approvals notifies when a version needs approval even if the rule
	// has no notify action. Default true.
	Approvals *bool `yaml:"approvals"`
	// RemindAfter re-sends a pending approval this long after the last
	// message about it. Default 72h; 0 turns reminders off.
	RemindAfter *time.Duration `yaml:"remind_after"`
	// SyncProblems notifies about merge conflicts and CI failures on sync
	// PRs. Default true.
	SyncProblems *bool `yaml:"sync_problems"`
	// PullRequests notifies when a sync PR is opened, updated with a newer
	// release, merged or closed. Default false.
	PullRequests bool `yaml:"pull_requests"`
}

// Ntfy publishes to an ntfy topic (https://ntfy.sh or self-hosted).
type Ntfy struct {
	// URL is the topic URL, e.g. https://ntfy.sh/my-topic.
	URL string `yaml:"url"`
	// Token is an optional credential name holding an access token.
	Token string `yaml:"token"`
}

// Gotify sends to a Gotify server.
type Gotify struct {
	URL string `yaml:"url"`
	// Token is the credential name of the application token.
	Token string `yaml:"token"`
}

// Email sends mail over SMTP with TLS (implicit on port 465, STARTTLS
// otherwise; plain connections are refused).
type Email struct {
	// SMTP is host:port.
	SMTP     string `yaml:"smtp"`
	Username string `yaml:"username"`
	// Password is the credential name of the SMTP password.
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

// Webhook POSTs each message as JSON, for chat bridges and scripts.
type Webhook struct {
	URL string `yaml:"url"`
	// Token is an optional credential name, sent as a bearer token.
	Token string `yaml:"token"`
}

// DefaultRemindAfter is how long a pending approval waits before a reminder.
const DefaultRemindAfter = 72 * time.Hour

// NotifyApprovals reports whether approvals are notified without a notify action.
func (n Notifications) NotifyApprovals() bool { return n.Approvals == nil || *n.Approvals }

// NotifySyncProblems reports whether conflicts and CI failures are notified.
func (n Notifications) NotifySyncProblems() bool { return n.SyncProblems == nil || *n.SyncProblems }

// Reminders returns the reminder interval; 0 means off.
func (n Notifications) Reminders() time.Duration {
	if n.RemindAfter == nil {
		return DefaultRemindAfter
	}
	return *n.RemindAfter
}

// Enabled reports whether any channel is configured.
func (n Notifications) Enabled() bool {
	return n.Ntfy != nil || n.Gotify != nil || n.Email != nil || n.Webhook != nil
}

func (n Notifications) credentialNames() []string {
	var out []string
	if n.Ntfy != nil && n.Ntfy.Token != "" {
		out = append(out, n.Ntfy.Token)
	}
	if n.Gotify != nil {
		out = append(out, n.Gotify.Token)
	}
	if n.Email != nil && n.Email.Password != "" {
		out = append(out, n.Email.Password)
	}
	if n.Webhook != nil && n.Webhook.Token != "" {
		out = append(out, n.Webhook.Token)
	}
	return out
}

func (c *Config) validateNotifications(add func(string, ...any)) {
	n := c.Notifications
	cred := func(field, name string, required bool) {
		if name == "" {
			if required {
				add("%s: required (a credential name)", field)
			}
			return
		}
		if err := credentials.ValidateName(name); err != nil {
			add("%s: %v", field, err)
		}
	}
	if n.Ntfy != nil {
		if u, err := notifyURL(n.Ntfy.URL); err != nil {
			add("notifications.ntfy.url: %v", err)
		} else if strings.Trim(u.Path, "/") == "" || strings.Contains(strings.Trim(u.Path, "/"), "/") {
			add("notifications.ntfy.url: %q must be a topic URL like https://ntfy.sh/my-topic", n.Ntfy.URL)
		}
		cred("notifications.ntfy.token", n.Ntfy.Token, false)
	}
	if n.Gotify != nil {
		if _, err := notifyURL(n.Gotify.URL); err != nil {
			add("notifications.gotify.url: %v", err)
		}
		cred("notifications.gotify.token", n.Gotify.Token, true)
	}
	if n.Email != nil {
		e := n.Email
		if _, port, err := net.SplitHostPort(e.SMTP); err != nil || port == "" {
			add("notifications.email.smtp: %q must be host:port", e.SMTP)
		}
		if _, err := mail.ParseAddress(e.From); err != nil {
			add("notifications.email.from: %q is not an email address", e.From)
		}
		if len(e.To) == 0 {
			add("notifications.email.to: at least one address is required")
		}
		for _, to := range e.To {
			if _, err := mail.ParseAddress(to); err != nil {
				add("notifications.email.to: %q is not an email address", to)
			}
		}
		if (e.Username == "") != (e.Password == "") {
			add("notifications.email: username and password go together")
		}
		cred("notifications.email.password", e.Password, false)
	}
	if n.Webhook != nil {
		if _, err := notifyURL(n.Webhook.URL); err != nil {
			add("notifications.webhook.url: %v", err)
		}
		cred("notifications.webhook.token", n.Webhook.Token, false)
	}
	if n.RemindAfter != nil && *n.RemindAfter != 0 && *n.RemindAfter < time.Hour {
		add("notifications.remind_after: must be 0 (off) or at least 1h")
	}
}

// notifyURL accepts https URLs, and http only for loopback and private
// addresses (a self-hosted server on the LAN or a tailnet), since tokens
// and message contents would otherwise cross the internet in the clear.
func notifyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, fmt.Errorf("%q must be an https:// URL without credentials or a query", raw)
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return u, nil
		}
		if ip, err := netip.ParseAddr(host); err == nil &&
			(ip.IsLoopback() || ip.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) {
			return u, nil
		}
		return nil, fmt.Errorf("%q: http:// is only allowed for localhost and private or Tailscale IP addresses; use https://", raw)
	}
	return nil, fmt.Errorf("%q must be an https:// URL", raw)
}
