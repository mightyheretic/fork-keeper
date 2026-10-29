package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// Forks implements provider.ForkHost: git access, pull requests and CI
// status on GitHub.
type Forks struct {
	api   *apiClient
	web   string
	token TokenFunc
	// ForkTimeout bounds how long CreateFork waits for GitHub to copy the repo.
	ForkTimeout time.Duration
	// ForkPoll is the delay between readiness checks.
	ForkPoll time.Duration
}

// NewForks returns a ForkHost. token supplies credentials per repo (nil for
// anonymous, which can only fetch public repos).
func NewForks(apiURL string, token TokenFunc, hc *http.Client) *Forks {
	return &Forks{api: newAPIClient(apiURL, hc), web: webURL(apiURL), token: token,
		ForkTimeout: 5 * time.Minute, ForkPoll: 3 * time.Second}
}

var _ provider.ForkHost = (*Forks)(nil)

// webURL derives the git host from the API URL: api.github.com ->
// github.com, and GitHub Enterprise's https://host/api/v3 -> https://host.
func webURL(apiURL string) string {
	u, err := url.Parse(strings.TrimRight(apiURL, "/"))
	if err != nil {
		return "https://github.com"
	}
	if u.Host == "api.github.com" {
		return "https://github.com"
	}
	u.Path = strings.TrimSuffix(u.Path, "/api/v3")
	return strings.TrimRight(u.String(), "/")
}

func (f *Forks) CloneURL(repo provider.Repo) string {
	return f.web + "/" + repo.Owner + "/" + repo.Name + ".git"
}

// GitAuth returns HTTP Basic credentials, which git over HTTPS accepts for
// both installation tokens and PATs.
func (f *Forks) GitAuth(ctx context.Context, repo provider.Repo) (credentials.Secret, error) {
	tok, err := f.bearer(ctx, repo)
	if err != nil || tok == "" {
		return credentials.Secret{}, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	return credentials.NewSecret([]byte("Basic " + basic)), nil
}

func (f *Forks) bearer(ctx context.Context, repo provider.Repo) (string, error) {
	if f.token == nil {
		return "", nil
	}
	tok, err := f.token(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("token for %s: %w", repo.FullName(), err)
	}
	return tok.Reveal(), nil
}

func (f *Forks) call(ctx context.Context, repo provider.Repo, method, path string, in, out any) error {
	bearer, err := f.bearer(ctx, repo)
	if err != nil {
		return err
	}
	_, err = f.api.do(ctx, method, repoPath(repo)+path, bearer, in, out)
	return err
}

func (f *Forks) DefaultBranch(ctx context.Context, repo provider.Repo) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := f.call(ctx, repo, http.MethodGet, "", nil, &r); err != nil {
		return "", notFound(err)
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("%s has no default branch", repo.FullName())
	}
	return r.DefaultBranch, nil
}

// notFound maps a 404 to provider.ErrRefNotFound.
func notFound(err error) error {
	if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.Status == http.StatusNotFound {
		return fmt.Errorf("%w: %w", provider.ErrRefNotFound, err)
	}
	return err
}

type pullJSON struct {
	Number   int    `json:"number"`
	HTMLURL  string `json:"html_url"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergedAt string `json:"merged_at"`
	Draft    bool   `json:"draft"`
	Head     struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (p pullJSON) pr() *provider.PullRequest {
	pr := &provider.PullRequest{
		Number: p.Number, URL: p.HTMLURL, State: p.State, Draft: p.Draft, HeadSHA: p.Head.SHA,
		// The list endpoint omits "merged" but includes merged_at.
		Merged: p.Merged || p.MergedAt != "",
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr
}

func (f *Forks) FindPullRequest(ctx context.Context, repo provider.Repo, head, base string) (*provider.PullRequest, error) {
	q := url.Values{"state": {"open"}, "head": {repo.Owner + ":" + head}, "base": {base}}
	var pulls []pullJSON
	if err := f.call(ctx, repo, http.MethodGet, "/pulls?"+q.Encode(), nil, &pulls); err != nil {
		return nil, err
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	return pulls[0].pr(), nil
}

func (f *Forks) CreatePullRequest(ctx context.Context, repo provider.Repo, in provider.PullRequestInput) (*provider.PullRequest, error) {
	body := map[string]any{"title": in.Title, "body": in.Body, "head": in.Head, "base": in.Base,
		"draft": in.Draft, "maintainer_can_modify": false}
	var p pullJSON
	if err := f.call(ctx, repo, http.MethodPost, "/pulls", body, &p); err != nil {
		return nil, err
	}
	return p.pr(), nil
}

func (f *Forks) UpdatePullRequest(ctx context.Context, repo provider.Repo, number int, in provider.PullRequestInput) (*provider.PullRequest, error) {
	var p pullJSON
	if err := f.call(ctx, repo, http.MethodPatch, fmt.Sprintf("/pulls/%d", number),
		map[string]any{"title": in.Title, "body": in.Body}, &p); err != nil {
		return nil, err
	}
	return p.pr(), nil
}

func (f *Forks) GetPullRequest(ctx context.Context, repo provider.Repo, number int) (*provider.PullRequest, error) {
	var p pullJSON
	if err := f.call(ctx, repo, http.MethodGet, fmt.Sprintf("/pulls/%d", number), nil, &p); err != nil {
		return nil, notFound(err)
	}
	return p.pr(), nil
}

func (f *Forks) AddLabels(ctx context.Context, repo provider.Repo, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	return f.call(ctx, repo, http.MethodPost, fmt.Sprintf("/issues/%d/labels", number),
		map[string]any{"labels": labels}, nil)
}

// CIStatus combines check runs (GitHub Actions and Apps) and commit
// statuses (older integrations) for sha.
func (f *Forks) CIStatus(ctx context.Context, repo provider.Repo, sha string) (provider.CIStatus, error) {
	var runs struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := f.call(ctx, repo, http.MethodGet, "/commits/"+url.PathEscape(sha)+"/check-runs?per_page=100", nil, &runs); err != nil {
		return provider.CIStatus{}, err
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := f.call(ctx, repo, http.MethodGet, "/commits/"+url.PathEscape(sha)+"/status?per_page=100", nil, &combined); err != nil {
		return provider.CIStatus{}, err
	}

	st := provider.CIStatus{State: provider.CINone}
	pending := false
	for _, r := range runs.CheckRuns {
		st.Total++
		switch {
		case r.Status != "completed":
			pending = true
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
		default:
			st.Failed = append(st.Failed, r.Name)
		}
	}
	// More runs than one page: treat the rest as unknown, so wait.
	if runs.TotalCount > len(runs.CheckRuns) {
		pending = true
	}
	for _, s := range combined.Statuses {
		st.Total++
		switch s.State {
		case "success":
		case "pending":
			pending = true
		default:
			st.Failed = append(st.Failed, s.Context)
		}
	}
	switch {
	case len(st.Failed) > 0:
		st.State = provider.CIFailure
	case pending:
		st.State = provider.CIPending
	case st.Total > 0:
		st.State = provider.CISuccess
	}
	return st, nil
}

func (f *Forks) MergePullRequest(ctx context.Context, repo provider.Repo, number int, sha, title string) error {
	return f.call(ctx, repo, http.MethodPut, fmt.Sprintf("/pulls/%d/merge", number),
		map[string]any{"sha": sha, "merge_method": "merge", "commit_title": title}, nil)
}

// CreateFork forks upstream into the token owner's account, or into org if
// set, and waits until GitHub has finished copying it. If the fork already
// exists, GitHub returns it. It returns the fork and its default branch.
func (f *Forks) CreateFork(ctx context.Context, upstream provider.Repo, org string) (provider.Repo, string, error) {
	body := map[string]any{}
	if org != "" {
		body["organization"] = org
	}
	var created struct {
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := f.call(ctx, upstream, http.MethodPost, "/forks", body, &created); err != nil {
		return provider.Repo{}, "", notFound(err)
	}
	fork := provider.Repo{Owner: created.Owner.Login, Name: created.Name}
	if fork.Owner == "" || fork.Name == "" {
		return provider.Repo{}, "", errors.New("GitHub did not return the new fork's name")
	}

	// Forking is asynchronous: the repo exists before its git data does.
	// Until then the commits endpoint answers 409 (empty) or 404.
	ctx, cancel := context.WithTimeout(ctx, f.ForkTimeout)
	defer cancel()
	for {
		var commits []json.RawMessage
		err := f.call(ctx, fork, http.MethodGet, "/commits?per_page=1", nil, &commits)
		if err == nil && len(commits) > 0 {
			return fork, created.DefaultBranch, nil
		}
		var apiErr *APIError
		if err != nil && !(errors.As(err, &apiErr) && (apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusNotFound)) {
			return fork, "", err
		}
		select {
		case <-ctx.Done():
			return fork, "", fmt.Errorf("fork %s was created but GitHub hasn't finished copying it: %w", fork.FullName(), ctx.Err())
		case <-time.After(f.ForkPoll):
		}
	}
}
