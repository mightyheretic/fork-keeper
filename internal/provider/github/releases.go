package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// MaxAssetBytes is GitHub's limit on one release file.
const MaxAssetBytes = 2 << 30

// releasePages bounds how far back ForkRelease looks for a tag's release.
const releasePages = 10

// ReleaseFiles implements provider.ReleaseHost on GitHub.
type ReleaseFiles struct {
	api   *apiClient
	token TokenFunc
	// dl moves release files: no overall timeout, since large files take
	// a while. Callers bound transfers with the context.
	dl *http.Client
}

// NewReleaseFiles returns a ReleaseHost. token may be nil for anonymous
// access, which can only read public releases.
func NewReleaseFiles(apiURL string, token TokenFunc, hc *http.Client) *ReleaseFiles {
	dl := hc
	if dl == nil {
		dl = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
		}}
	}
	return &ReleaseFiles{api: newAPIClient(apiURL, hc), token: token, dl: dl}
}

var _ provider.ReleaseHost = (*ReleaseFiles)(nil)

type ghRelease struct {
	ID        int64     `json:"id"`
	TagName   string    `json:"tag_name"`
	Draft     bool      `json:"draft"`
	HTMLURL   string    `json:"html_url"`
	UploadURL string    `json:"upload_url"`
	Assets    []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	ContentType string  `json:"content_type"`
	Digest      *string `json:"digest"`
}

func (a ghAsset) asset() provider.ReleaseAsset {
	out := provider.ReleaseAsset{ID: a.ID, Name: a.Name, Size: a.Size, ContentType: a.ContentType}
	if a.Digest != nil {
		out.Digest = *a.Digest
	}
	return out
}

func (r ghRelease) release() *provider.Release {
	out := &provider.Release{ID: r.ID, TagName: r.TagName, Draft: r.Draft, URL: r.HTMLURL,
		UploadURL: strings.TrimSuffix(r.UploadURL, "{?name,label}")}
	for _, a := range r.Assets {
		out.Assets = append(out.Assets, a.asset())
	}
	return out
}

func (f *ReleaseFiles) bearer(ctx context.Context, repo provider.Repo) (string, error) {
	if f.token == nil {
		return "", nil
	}
	tok, err := f.token(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("token for %s: %w", repo.FullName(), err)
	}
	return tok.Reveal(), nil
}

func (f *ReleaseFiles) call(ctx context.Context, repo provider.Repo, method, path string, in, out any) error {
	bearer, err := f.bearer(ctx, repo)
	if err != nil {
		return err
	}
	_, err = f.api.do(ctx, method, path, bearer, in, out)
	return err
}

func (f *ReleaseFiles) UpstreamRelease(ctx context.Context, repo provider.Repo, tag string) (*provider.Release, error) {
	var r ghRelease
	err := f.call(ctx, repo, http.MethodGet, repoPath(repo)+"/releases/tags/"+escapeRef(tag), nil, &r)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil, fmt.Errorf("release %s of %s: %w", tag, repo.FullName(), provider.ErrRefNotFound)
	}
	if err != nil {
		return nil, err
	}
	return r.release(), nil
}

// ForkRelease lists releases, since the by-tag endpoint skips drafts.
func (f *ReleaseFiles) ForkRelease(ctx context.Context, repo provider.Repo, tag string) (*provider.Release, error) {
	for page := 1; page <= releasePages; page++ {
		var rs []ghRelease
		path := fmt.Sprintf("%s/releases?per_page=100&page=%d", repoPath(repo), page)
		if err := f.call(ctx, repo, http.MethodGet, path, nil, &rs); err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.TagName == tag {
				return r.release(), nil
			}
		}
		if len(rs) < 100 {
			break
		}
	}
	return nil, nil
}

func (f *ReleaseFiles) CreateRelease(ctx context.Context, repo provider.Repo, in provider.ReleaseInput) (*provider.Release, error) {
	body := map[string]any{"tag_name": in.TagName, "name": in.Name, "body": in.Body, "draft": in.Draft,
		"make_latest": "false"}
	if !in.Draft && in.Commit != "" {
		body["target_commitish"] = in.Commit
	}
	var r ghRelease
	if err := f.call(ctx, repo, http.MethodPost, repoPath(repo)+"/releases", body, &r); err != nil {
		return nil, err
	}
	return r.release(), nil
}

func (f *ReleaseFiles) GetAsset(ctx context.Context, repo provider.Repo, id int64) (provider.ReleaseAsset, error) {
	var a ghAsset
	if err := f.call(ctx, repo, http.MethodGet, repoPath(repo)+"/releases/assets/"+strconv.FormatInt(id, 10), nil, &a); err != nil {
		return provider.ReleaseAsset{}, err
	}
	return a.asset(), nil
}

func (f *ReleaseFiles) DeleteAsset(ctx context.Context, repo provider.Repo, id int64) error {
	return f.call(ctx, repo, http.MethodDelete, repoPath(repo)+"/releases/assets/"+strconv.FormatInt(id, 10), nil, nil)
}

// DownloadAsset follows GitHub's redirect to its file storage; Go's client
// drops the Authorization header on that cross-host redirect.
func (f *ReleaseFiles) DownloadAsset(ctx context.Context, repo provider.Repo, id int64) (io.ReadCloser, error) {
	bearer, err := f.bearer(ctx, repo)
	if err != nil {
		return nil, err
	}
	p := repoPath(repo) + "/releases/assets/" + strconv.FormatInt(id, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.api.base+p, nil)
	if err != nil {
		return nil, err
	}
	f.headers(req, bearer)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.dl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download release file %d of %s: %w", id, repo.FullName(), err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, f.httpError(resp, http.MethodGet, p)
	}
	return &limitedBody{r: io.LimitReader(resp.Body, MaxAssetBytes+1), c: resp.Body, left: MaxAssetBytes}, nil
}

func (f *ReleaseFiles) UploadAsset(ctx context.Context, repo provider.Repo, rel *provider.Release, name, contentType string,
	size int64, body io.Reader) (provider.ReleaseAsset, error) {
	if rel.UploadURL == "" {
		return provider.ReleaseAsset{}, fmt.Errorf("release %d of %s has no upload URL", rel.ID, repo.FullName())
	}
	bearer, err := f.bearer(ctx, repo)
	if err != nil {
		return provider.ReleaseAsset{}, err
	}
	u := rel.UploadURL + "?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return provider.ReleaseAsset{}, err
	}
	f.headers(req, bearer)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = size
	resp, err := f.dl.Do(req)
	if err != nil {
		return provider.ReleaseAsset{}, fmt.Errorf("upload %s to %s: %w", name, repo.FullName(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return provider.ReleaseAsset{}, f.httpError(resp, http.MethodPost, "upload "+name)
	}
	var a ghAsset
	if err := decodeJSON(resp.Body, &a); err != nil {
		return provider.ReleaseAsset{}, fmt.Errorf("upload %s to %s: %w", name, repo.FullName(), err)
	}
	return a.asset(), nil
}

func (f *ReleaseFiles) headers(req *http.Request, bearer string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", f.api.userAgent)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
}

func (f *ReleaseFiles) httpError(resp *http.Response, method, path string) error {
	if rl := rateLimitError(resp); rl != nil {
		return rl
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("github %s %s: %w", method, path, provider.ErrRefNotFound)
	}
	var e struct {
		Message string `json:"message"`
	}
	_ = decodeJSON(resp.Body, &e)
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: e.Message}
}

func decodeJSON(r io.Reader, out any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxRespBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
