package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// MaxArchiveBytes caps a downloaded source tarball (compressed).
const MaxArchiveBytes = 2 << 30

// Archives downloads source tarballs of a repo at a ref.
type Archives struct {
	api   *apiClient
	token TokenFunc
	// dl has no overall timeout: large repos take a while. Callers bound
	// the download with the context.
	dl *http.Client
}

// NewArchives returns an ArchiveFetcher. token may be nil for anonymous access.
func NewArchives(apiURL string, token TokenFunc) *Archives {
	return &Archives{
		api:   newAPIClient(apiURL, nil),
		token: token,
		dl: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
		}},
	}
}

var _ provider.ArchiveFetcher = (*Archives)(nil)

// Archive returns a gzipped tarball of repo at ref. Its entries sit under
// one top-level directory. GitHub redirects to codeload.github.com; Go's
// client drops the Authorization header on that cross-host redirect.
func (a *Archives) Archive(ctx context.Context, repo provider.Repo, ref string) (io.ReadCloser, error) {
	var bearer string
	if a.token != nil {
		tok, err := a.token(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("token for %s: %w", repo.FullName(), err)
		}
		bearer = tok.Reveal()
	}
	p := repoPath(repo) + "/tarball/" + escapeRef(ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.api.base+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", a.api.userAgent)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := a.dl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s@%s: %w", repo.FullName(), ref, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if rl := rateLimitError(resp); rl != nil {
			return nil, rl
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("download %s@%s: %w", repo.FullName(), ref, provider.ErrRefNotFound)
		}
		return nil, &APIError{Status: resp.StatusCode, Method: http.MethodGet, Path: p, Message: http.StatusText(resp.StatusCode)}
	}
	return &limitedBody{r: io.LimitReader(resp.Body, MaxArchiveBytes+1), c: resp.Body, left: MaxArchiveBytes}, nil
}

// escapeRef escapes each segment of a ref, keeping "/" (as in "release/1.0").
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// limitedBody fails, rather than silently truncating, past MaxArchiveBytes.
type limitedBody struct {
	r    io.Reader
	c    io.Closer
	left int64
}

var errArchiveTooLarge = errors.New("source archive exceeds size limit")

func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, errArchiveTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.c.Close() }
