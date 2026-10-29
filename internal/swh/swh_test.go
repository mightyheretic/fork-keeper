package swh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

func TestSave(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		if r.URL.Path == "/api/1/origin/save/git/url/https://github.com/limited/repo/" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"exception":"Throttled","reason":"Request was throttled."}`))
			return
		}
		w.Write([]byte(`{"id":42,"save_request_status":"accepted","save_task_status":"not yet scheduled"}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/1", nil, nil)
	sr, err := c.Save(context.Background(), "https://github.com/up/app")
	if err != nil || sr.ID != 42 || sr.Status != "accepted" {
		t.Fatalf("save = %+v, %v", sr, err)
	}
	if path != "/api/1/origin/save/git/url/https://github.com/up/app/" || auth != "" {
		t.Fatalf("path %q auth %q", path, auth)
	}

	c = New(srv.URL+"/api/1", func(context.Context) (credentials.Secret, error) {
		return credentials.NewSecret([]byte("swh-token")), nil
	}, nil)
	if _, err := c.Save(context.Background(), "https://github.com/up/app"); err != nil || auth != "Bearer swh-token" {
		t.Fatalf("token: %q %v", auth, err)
	}
	var e *Error
	if _, err := c.Save(context.Background(), "https://github.com/limited/repo"); !errors.As(err, &e) || e.Status != 429 {
		t.Fatalf("throttled: %v", err)
	}
	for _, bad := range []string{"http://github.com/up/app", "https://user:pw@github.com/up/app", "file:///etc"} {
		if _, err := c.Save(context.Background(), bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
