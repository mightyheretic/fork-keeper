package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
)

const (
	releasesPerPage = 30
	tagsPerPage     = 100
	// maxTagPages bounds a tag poll. The tags endpoint isn't ordered by
	// date, so a new tag can land on any page and there is no safe ETag.
	maxTagPages = 3
)

// TokenFunc returns a token for reading repo. An empty Secret means
// anonymous access.
type TokenFunc func(ctx context.Context, repo provider.Repo) (credentials.Secret, error)

// Refs lists upstream releases and tags through the REST API.
type Refs struct {
	api   *apiClient
	token TokenFunc
}

// NewRefs returns a RefLister. token may be nil for anonymous access, which
// GitHub limits to 60 requests an hour.
func NewRefs(apiURL string, token TokenFunc, hc *http.Client) *Refs {
	return &Refs{api: newAPIClient(apiURL, hc), token: token}
}

var _ provider.RefLister = (*Refs)(nil)

func (r *Refs) bearer(ctx context.Context, repo provider.Repo) (string, error) {
	if r.token == nil {
		return "", nil
	}
	tok, err := r.token(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("token for %s: %w", repo.FullName(), err)
	}
	return tok.Reveal(), nil
}

func repoPath(repo provider.Repo) string {
	return "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

func (r *Refs) ListReleases(ctx context.Context, repo provider.Repo, etag string) (provider.RefList, error) {
	bearer, err := r.bearer(ctx, repo)
	if err != nil {
		return provider.RefList{}, err
	}
	var releases []struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"published_at"`
		HTMLURL     string    `json:"html_url"`
	}
	resp, err := r.api.send(ctx, request{
		method: http.MethodGet,
		path:   fmt.Sprintf("%s/releases?per_page=%d", repoPath(repo), releasesPerPage),
		bearer: bearer,
		etag:   etag,
	}, &releases)
	list := provider.RefList{RateLimit: parseRateLimit(resp.header)}
	if err != nil {
		return list, err
	}
	list.ETag = resp.header.Get("ETag")
	if resp.notModified {
		list.NotModified = true
		list.ETag = etag
		return list, nil
	}
	for _, rel := range releases {
		if rel.Draft || rel.TagName == "" {
			continue
		}
		list.Refs = append(list.Refs, provider.Ref{
			Kind:        provider.KindRelease,
			Name:        rel.TagName,
			Title:       rel.Name,
			Prerelease:  rel.Prerelease,
			PublishedAt: rel.PublishedAt,
			URL:         rel.HTMLURL,
		})
	}
	return list, nil
}

func (r *Refs) ListTags(ctx context.Context, repo provider.Repo) (provider.RefList, error) {
	bearer, err := r.bearer(ctx, repo)
	if err != nil {
		return provider.RefList{}, err
	}
	var list provider.RefList
	for page := 1; page <= maxTagPages; page++ {
		var tags []struct {
			Name string `json:"name"`
		}
		resp, err := r.api.send(ctx, request{
			method: http.MethodGet,
			path:   fmt.Sprintf("%s/tags?per_page=%d&page=%d", repoPath(repo), tagsPerPage, page),
			bearer: bearer,
		}, &tags)
		list.RateLimit = parseRateLimit(resp.header)
		if err != nil {
			return list, err
		}
		for _, t := range tags {
			list.Refs = append(list.Refs, provider.Ref{Kind: provider.KindTag, Name: t.Name})
		}
		if len(tags) < tagsPerPage {
			break
		}
	}
	return list, nil
}
