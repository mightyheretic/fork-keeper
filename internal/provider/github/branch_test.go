package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func TestBranchReader(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	const blob = "89abcdef0123456789abcdef0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s: missing auth", r.URL)
		}
		switch r.URL.Path {
		case "/repos/up/app/commits/HEAD":
			if r.Header.Get("Accept") != "application/vnd.github.sha" {
				t.Errorf("accept = %q", r.Header.Get("Accept"))
			}
			if r.Header.Get("If-None-Match") == `"e1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"e1"`)
			w.Write([]byte(head))
		case "/repos/up/app/contents":
			if r.URL.Query().Get("ref") != head {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			w.Write([]byte(`[{"path":"LICENSE","type":"file","sha":"` + blob + `","size":3},
				{"path":"LICENSES","type":"dir","sha":"x"},{"path":"link","type":"symlink","sha":"y"}]`))
		case "/repos/up/app/contents/LICENSES":
			w.Write([]byte(`[{"path":"LICENSES/MIT.txt","type":"file","sha":"` + blob + `","size":3}]`))
		case "/repos/up/app/git/blobs/" + blob:
			w.Write([]byte(`{"content":"` + base64.StdEncoding.EncodeToString([]byte("MIT")) + `\n","encoding":"base64","size":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	r := NewRefs(srv.URL, staticToken("tok"), nil)
	repo := provider.Repo{Owner: "up", Name: "app"}
	ctx := context.Background()

	h, err := r.DefaultHead(ctx, repo, "")
	if err != nil || h.SHA != head || h.ETag != `"e1"` {
		t.Fatalf("head = %+v, %v", h, err)
	}
	if h, err = r.DefaultHead(ctx, repo, `"e1"`); err != nil || !h.NotModified {
		t.Fatalf("conditional head = %+v, %v", h, err)
	}
	files, err := r.ListFiles(ctx, repo, head, "")
	if err != nil || len(files) != 2 || files[0].ID != blob || files[1].Path != "LICENSES/" {
		t.Fatalf("root = %+v, %v", files, err)
	}
	if files, err = r.ListFiles(ctx, repo, head, "LICENSES"); err != nil || len(files) != 1 || files[0].Path != "LICENSES/MIT.txt" {
		t.Fatalf("LICENSES = %+v, %v", files, err)
	}
	b, err := r.ReadFile(ctx, repo, blob)
	if err != nil || string(b) != "MIT" {
		t.Fatalf("blob = %q, %v", b, err)
	}
	if _, err := r.ReadFile(ctx, repo, "../../etc"); err == nil {
		t.Fatal("non-SHA blob ID accepted")
	}
}
