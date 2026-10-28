package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// do sends a request and decodes a JSON response into out (if non-nil).
// The bearer value is never included in errors.
func (c *apiClient) do(ctx context.Context, method, path, bearer string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("github %s %s: read response: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return resp.Header, &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: e.Message}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("github %s %s: decode response: %w", method, path, err)
		}
	}
	return resp.Header, nil
}
