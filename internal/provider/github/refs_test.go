package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func TestListReleasesConditional(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		if r.URL.Path != "/repos/up/app/releases" || r.URL.Query().Get("per_page") != "30" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "1793000000")
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, `[
			{"tag_name":"v2.0.0-rc1","name":"RC","prerelease":true,"published_at":"2026-10-20T00:00:00Z"},
			{"tag_name":"v1.9.0","name":"","draft":true},
			{"tag_name":"v1.8.0","name":"Stable","published_at":"2026-09-01T00:00:00Z","html_url":"https://example/r/1.8"}
		]`)
	}))
	defer srv.Close()

	token := func(context.Context, provider.Repo) (credentials.Secret, error) {
		return credentials.NewSecret([]byte("tok")), nil
	}
	r := NewRefs(srv.URL, token, nil)
	ctx := context.Background()

	list, err := r.ListReleases(ctx, upApp, "")
	if err != nil {
		t.Fatal(err)
	}
	if list.ETag != `"v1"` || len(list.Refs) != 2 {
		t.Fatalf("etag=%q refs=%+v (draft should be skipped)", list.ETag, list.Refs)
	}
	if rc := list.Refs[0]; rc.Name != "v2.0.0-rc1" || !rc.Prerelease || rc.PublishedAt.IsZero() {
		t.Fatalf("first ref = %+v", rc)
	}
	if list.Refs[1].URL != "https://example/r/1.8" || list.RateLimit.Remaining != 4999 {
		t.Fatalf("ref/rate = %+v / %+v", list.Refs[1], list.RateLimit)
	}

	list, err = r.ListReleases(ctx, upApp, `"v1"`)
	if err != nil || !list.NotModified || list.ETag != `"v1"` || len(list.Refs) != 0 {
		t.Fatalf("conditional: list=%+v err=%v", list, err)
	}
	if gotAuth[0] != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth[0])
	}
}

func TestListReleasesAnonymous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("anonymous request sent Authorization %q", h)
		}
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	anon := func(context.Context, provider.Repo) (credentials.Secret, error) { return credentials.Secret{}, nil }
	for _, tok := range []TokenFunc{nil, anon} {
		if _, err := NewRefs(srv.URL, tok, nil).ListReleases(context.Background(), upApp, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRateLimitErrors(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	tests := map[string]http.HandlerFunc{
		"primary": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		},
		"secondary": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1200")
			http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusTooManyRequests)
		},
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			_, err := NewRefs(srv.URL, nil, nil).ListReleases(context.Background(), upApp, "")
			var rl *provider.RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want RateLimitError", err)
			}
			if d := rl.Reset.Sub(reset); d < -5*time.Second || d > 5*time.Second {
				t.Fatalf("reset = %v, want about %v", rl.Reset, reset)
			}
		})
	}

	// A plain 403 (e.g. no access) is not a rate limit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := NewRefs(srv.URL, nil, nil).ListReleases(context.Background(), upApp, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 403 {
		t.Fatalf("err = %v, want APIError 403", err)
	}
}

func TestListTagsPaginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		n := 100
		if page == "2" {
			n = 5
		}
		fmt.Fprint(w, "[")
		for i := 0; i < n; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"name":"p%s-t%d"}`, page, i)
		}
		fmt.Fprint(w, "]")
	}))
	defer srv.Close()

	list, err := NewRefs(srv.URL, nil, nil).ListTags(context.Background(), upApp)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Refs) != 105 || len(pages) != 2 || list.Refs[0].Kind != provider.KindTag {
		t.Fatalf("refs=%d pages=%v", len(list.Refs), pages)
	}
}
