package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

const (
	apiVersion   = "2022-11-28"
	maxRespBytes = 4 << 20
)

// apiClient is a minimal GitHub REST client. Auth is passed per request
// because App endpoints use a JWT while repo endpoints use a token.
type apiClient struct {
	base      string
	http      *http.Client
	userAgent string
}

func newAPIClient(baseURL string, hc *http.Client) *apiClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &apiClient{base: strings.TrimRight(baseURL, "/"), http: hc, userAgent: "fork-keeper"}
}

// APIError is a non-2xx response from GitHub.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// request describes one API call.
type request struct {
	method string
	path   string
	bearer string // never included in errors
	body   any
	etag   string // sent as If-None-Match
	accept string // default application/vnd.github+json
}

// response is a completed API call.
type response struct {
	header      http.Header
	notModified bool
}

// do sends a request and decodes a JSON response into out (if non-nil).
func (c *apiClient) do(ctx context.Context, method, path, bearer string, in, out any) (http.Header, error) {
	resp, err := c.send(ctx, request{method: method, path: path, bearer: bearer, body: in}, out)
	return resp.header, err
}

func (c *apiClient) send(ctx context.Context, r request, out any) (response, error) {
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return response{}, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, body)
	if err != nil {
		return response{}, err
	}
	accept := r.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.etag != "" {
		req.Header.Set("If-None-Match", r.etag)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("github %s %s: %w", r.method, r.path, err)
	}
	defer httpResp.Body.Close()
	resp := response{header: httpResp.Header}
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, maxRespBytes))
	if err != nil {
		return resp, fmt.Errorf("github %s %s: read response: %w", r.method, r.path, err)
	}

	if httpResp.StatusCode == http.StatusNotModified && r.etag != "" {
		resp.notModified = true
		return resp, nil
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		if rl := rateLimitError(httpResp); rl != nil {
			return resp, rl
		}
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = http.StatusText(httpResp.StatusCode)
		}
		return resp, &APIError{Status: httpResp.StatusCode, Method: r.method, Path: r.path, Message: e.Message}
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return resp, nil
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, fmt.Errorf("github %s %s: decode response: %w", r.method, r.path, err)
		}
	}
	return resp, nil
}

// rateLimitError recognizes GitHub's primary (403/429 with no remaining
// quota) and secondary (Retry-After) rate limits.
func rateLimitError(resp *http.Response) *provider.RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			return &provider.RateLimitError{Reset: time.Now().Add(time.Duration(secs) * time.Second)}
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return &provider.RateLimitError{Reset: parseRateLimit(resp.Header).Reset}
	}
	return nil
}

func parseRateLimit(h http.Header) provider.RateLimit {
	var rl provider.RateLimit
	rl.Limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit"))
	rl.Remaining, _ = strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		rl.Reset = time.Unix(reset, 0)
	}
	return rl
}
