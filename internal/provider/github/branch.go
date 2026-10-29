package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// maxFileBytes caps a file read for the early warning. License files and
// manifests are far smaller.
const maxFileBytes = 1 << 20

var _ provider.BranchReader = (*Refs)(nil)

// DefaultHead reads the head of the default branch ("HEAD" resolves to it)
// as a bare SHA. A matching ETag gives a 304, which doesn't count against
// the rate limit.
func (r *Refs) DefaultHead(ctx context.Context, repo provider.Repo, etag string) (provider.BranchHead, error) {
	bearer, err := r.bearer(ctx, repo)
	if err != nil {
		return provider.BranchHead{}, err
	}
	var raw []byte
	resp, err := r.api.send(ctx, request{
		method: http.MethodGet,
		path:   repoPath(repo) + "/commits/HEAD",
		bearer: bearer,
		etag:   etag,
		accept: "application/vnd.github.sha",
	}, &raw)
	head := provider.BranchHead{RateLimit: parseRateLimit(resp.header)}
	if err != nil {
		return head, notFound(err)
	}
	if resp.notModified {
		head.NotModified, head.ETag = true, etag
		return head, nil
	}
	head.SHA = strings.TrimSpace(string(raw))
	if !isSHA(head.SHA) {
		return head, fmt.Errorf("github: unexpected head commit %q for %s", head.SHA, repo.FullName())
	}
	head.ETag = resp.header.Get("ETag")
	return head, nil
}

func (r *Refs) ListFiles(ctx context.Context, repo provider.Repo, commit, dir string) ([]provider.TreeFile, error) {
	bearer, err := r.bearer(ctx, repo)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Path string `json:"path"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
		Size int64  `json:"size"`
	}
	p := repoPath(repo) + "/contents"
	if dir != "" {
		p += "/" + escapeRef(dir)
	}
	if _, err := r.api.do(ctx, http.MethodGet, p+"?ref="+url.QueryEscape(commit), bearer, nil, &entries); err != nil {
		return nil, notFound(err)
	}
	out := make([]provider.TreeFile, 0, len(entries))
	for _, e := range entries {
		switch e.Type {
		case "file":
			out = append(out, provider.TreeFile{Path: e.Path, ID: e.SHA, Size: e.Size})
		case "dir":
			out = append(out, provider.TreeFile{Path: e.Path + "/"})
		}
	}
	return out, nil
}

func (r *Refs) ReadFile(ctx context.Context, repo provider.Repo, id string) ([]byte, error) {
	if !isSHA(id) {
		return nil, fmt.Errorf("invalid blob id %q", id)
	}
	bearer, err := r.bearer(ctx, repo)
	if err != nil {
		return nil, err
	}
	var blob struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	if _, err := r.api.do(ctx, http.MethodGet, repoPath(repo)+"/git/blobs/"+id, bearer, nil, &blob); err != nil {
		return nil, notFound(err)
	}
	if blob.Size > maxFileBytes {
		return nil, fmt.Errorf("%s blob %s is too large (%d bytes)", repo.FullName(), id, blob.Size)
	}
	if blob.Encoding != "base64" {
		return []byte(blob.Content), nil
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
}

func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
