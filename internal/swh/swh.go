// Package swh asks Software Heritage (archive.softwareheritage.org) to
// archive a public git repository, through its "save code now" API.
package swh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

// Client talks to the Software Heritage API.
type Client struct {
	base  string
	http  *http.Client
	token func(ctx context.Context) (credentials.Secret, error)
}

// New returns a client for the API at base (e.g.
// https://archive.softwareheritage.org/api/1). token may be nil: anonymous
// requests work, with a lower rate limit.
func New(base string, token func(ctx context.Context) (credentials.Secret, error), hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc, token: token}
}

// SaveRequest is Software Heritage's answer to a save request.
type SaveRequest struct {
	ID int64 `json:"id"`
	// Status is accepted, rejected or pending (pending manual review, for
	// hosts Software Heritage doesn't know).
	Status string `json:"save_request_status"`
	// TaskStatus is not created, pending, scheduled, running, succeeded or failed.
	TaskStatus string `json:"save_task_status"`
}

// Error is a non-2xx answer.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("Software Heritage: %d %s", e.Status, e.Message)
}

// Save asks Software Heritage to archive the git repository at originURL.
func (c *Client) Save(ctx context.Context, originURL string) (SaveRequest, error) {
	u, err := url.Parse(originURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return SaveRequest{}, fmt.Errorf("Software Heritage: %q is not a public https:// repository URL", originURL)
	}
	// The origin URL goes into the path as is, per the API docs.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/origin/save/git/url/"+originURL+"/", nil)
	if err != nil {
		return SaveRequest{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fork-keeper")
	if c.token != nil {
		tok, err := c.token(ctx)
		if err != nil {
			return SaveRequest{}, fmt.Errorf("Software Heritage token: %w", err)
		}
		if !tok.Empty() {
			req.Header.Set("Authorization", "Bearer "+tok.Reveal())
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return SaveRequest{}, fmt.Errorf("Software Heritage: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return SaveRequest{}, fmt.Errorf("Software Heritage: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Reason == "" {
			e.Reason = http.StatusText(resp.StatusCode)
		}
		return SaveRequest{}, &Error{Status: resp.StatusCode, Message: e.Reason}
	}
	var sr SaveRequest
	if err := json.Unmarshal(data, &sr); err != nil {
		return SaveRequest{}, fmt.Errorf("Software Heritage: decode response: %w", err)
	}
	return sr, nil
}
