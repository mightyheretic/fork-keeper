package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func staticToken(tok string) TokenFunc {
	return func(context.Context, provider.Repo) (credentials.Secret, error) {
		return credentials.NewSecret([]byte(tok)), nil
	}
}

var fork = provider.Repo{Owner: "me", Name: "app"}

func TestWebURLAndGitAuth(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":          "https://github.com",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com",
	} {
		if got := webURL(api); got != want {
			t.Errorf("webURL(%q) = %q, want %q", api, got, want)
		}
	}
	f := NewForks("https://api.github.com", staticToken("ghs_abc"), nil)
	if u := f.CloneURL(fork); u != "https://github.com/me/app.git" {
		t.Fatalf("clone URL = %q", u)
	}
	auth, err := f.GitAuth(context.Background(), fork)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth.Reveal(), "Basic "))
	if string(raw) != "x-access-token:ghs_abc" {
		t.Fatalf("basic auth = %q", raw)
	}
	if anon, _ := NewForks("https://api.github.com", nil, nil).GitAuth(context.Background(), fork); !anon.Empty() {
		t.Fatal("anonymous should have no auth")
	}
}

func TestPullRequests(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/me/app/pulls":
			q := r.URL.Query()
			if q.Get("head") != "me:forkkeeper/staging" || q.Get("base") != "main" || q.Get("state") != "open" {
				t.Errorf("query = %v", q)
			}
			w.Write([]byte(`[]`))
		case "POST /repos/me/app/pulls":
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"number": 7, "html_url": "https://github.com/me/app/pull/7", "state": "open", "draft": true,
				"head": {"sha": "abc"}, "labels": [{"name": "forkkeeper"}]}`))
		case "GET /repos/me/app/pulls/7":
			w.Write([]byte(`{"number": 7, "state": "closed", "merged": true, "head": {"sha": "abc"}}`))
		case "GET /repos/me/app/pulls/8":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "Not Found"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	f := NewForks(srv.URL, staticToken("tok"), nil)
	ctx := context.Background()

	if pr, err := f.FindPullRequest(ctx, fork, "forkkeeper/staging", "main"); err != nil || pr != nil {
		t.Fatalf("find = %+v, %v", pr, err)
	}
	pr, err := f.CreatePullRequest(ctx, fork, provider.PullRequestInput{Title: "t", Body: "b", Head: "forkkeeper/staging", Base: "main", Draft: true})
	if err != nil || pr.Number != 7 || !pr.Draft || pr.HeadSHA != "abc" || pr.Labels[0] != "forkkeeper" {
		t.Fatalf("create = %+v, %v", pr, err)
	}
	if created["draft"] != true || created["maintainer_can_modify"] != false || created["head"] != "forkkeeper/staging" {
		t.Fatalf("create body = %v", created)
	}
	if pr, err := f.GetPullRequest(ctx, fork, 7); err != nil || !pr.Merged {
		t.Fatalf("get = %+v, %v", pr, err)
	}
	if _, err := f.GetPullRequest(ctx, fork, 8); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("missing PR: %v", err)
	}
}

func TestCIStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		runs, statuses string
		want           provider.CIState
	}{
		"none":         {`{"total_count":0,"check_runs":[]}`, `{"statuses":[]}`, provider.CINone},
		"success":      {`{"total_count":2,"check_runs":[{"name":"a","status":"completed","conclusion":"success"},{"name":"b","status":"completed","conclusion":"skipped"}]}`, `{"statuses":[{"context":"ci/x","state":"success"}]}`, provider.CISuccess},
		"running":      {`{"total_count":1,"check_runs":[{"name":"a","status":"in_progress"}]}`, `{"statuses":[]}`, provider.CIPending},
		"status-pend":  {`{"total_count":0,"check_runs":[]}`, `{"statuses":[{"context":"ci/x","state":"pending"}]}`, provider.CIPending},
		"failed":       {`{"total_count":2,"check_runs":[{"name":"a","status":"in_progress"},{"name":"test","status":"completed","conclusion":"failure"}]}`, `{"statuses":[]}`, provider.CIFailure},
		"more-than-pg": {`{"total_count":150,"check_runs":[{"name":"a","status":"completed","conclusion":"success"}]}`, `{"statuses":[]}`, provider.CIPending},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/me/app/commits/abc/check-runs":
					w.Write([]byte(tc.runs))
				case "/repos/me/app/commits/abc/status":
					w.Write([]byte(tc.statuses))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			st, err := NewForks(srv.URL, nil, nil).CIStatus(context.Background(), fork, "abc")
			if err != nil || st.State != tc.want {
				t.Fatalf("state = %s (%+v), %v; want %s", st.State, st, err, tc.want)
			}
			if tc.want == provider.CIFailure && (len(st.Failed) != 1 || st.Failed[0] != "test") {
				t.Fatalf("failed = %v", st.Failed)
			}
		})
	}
}

func TestCreateForkWaitsUntilReady(t *testing.T) {
	polls := 0
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/up/app/forks":
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"name": "app", "default_branch": "main", "owner": {"login": "me"}}`))
		case "GET /repos/me/app/commits":
			polls++
			if polls < 3 {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"message": "Git Repository is empty."}`))
				return
			}
			w.Write([]byte(`[{"sha": "abc"}]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	f := NewForks(srv.URL, staticToken("tok"), nil)
	f.ForkPoll = time.Millisecond
	got, branch, err := f.CreateFork(context.Background(), provider.Repo{Owner: "up", Name: "app"}, "")
	if err != nil || got.FullName() != "me/app" || branch != "main" || polls != 3 {
		t.Fatalf("fork = %v %q %v after %d polls", got, branch, err, polls)
	}
	if len(body) != 0 {
		t.Fatalf("body = %v, want no organization", body)
	}
}

func TestApprovalPRCalls(t *testing.T) {
	var calls []string
	var gql map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /repos/me/app/pulls/7":
			w.Write([]byte(`{"number": 7, "node_id": "PR_kwABC", "state": "open", "draft": true, "head": {"sha": "abc"}}`))
		case "POST /graphql":
			json.NewDecoder(r.Body).Decode(&gql)
			w.Write([]byte(`{"data": {"markPullRequestReadyForReview": {"clientMutationId": null}}}`))
		case "DELETE /repos/me/app/issues/7/labels/forkkeeper:%20needs%20approval":
			w.Write([]byte(`[]`))
		case "DELETE /repos/me/app/issues/7/labels/forkkeeper:%20blocked":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "Label does not exist"}`))
		case "POST /repos/me/app/issues/7/comments":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case "PATCH /repos/me/app/pulls/7":
			w.Write([]byte(`{"number": 7, "state": "closed"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	f := NewForks(srv.URL, staticToken("tok"), nil)
	ctx := context.Background()
	if err := f.MarkReadyForReview(ctx, fork, 7); err != nil {
		t.Fatal(err)
	}
	if vars := gql["variables"].(map[string]any); vars["id"] != "PR_kwABC" {
		t.Fatalf("graphql = %v", gql)
	}
	for _, l := range []string{"forkkeeper: needs approval", "forkkeeper: blocked"} {
		if err := f.RemoveLabel(ctx, fork, 7, l); err != nil {
			t.Fatalf("remove %q: %v", l, err)
		}
	}
	if err := f.Comment(ctx, fork, 7, "approved"); err != nil {
		t.Fatal(err)
	}
	if err := f.ClosePullRequest(ctx, fork, 7); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 6 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestGraphQLBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":          "https://api.github.com",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com/api",
	} {
		if got := graphQLBase(in); got != want {
			t.Errorf("graphQLBase(%q) = %q, want %q", in, got, want)
		}
	}
}
