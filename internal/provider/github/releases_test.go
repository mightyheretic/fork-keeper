package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func TestReleaseFiles(t *testing.T) {
	var created []map[string]any
	// File storage is on another host, as on GitHub.
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("token sent to file storage")
		}
		w.Write([]byte("data"))
	}))
	defer storage.Close()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s: no auth", r.Method, r.URL.Path)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/up/app/releases/tags/v1":
			fmt.Fprintf(w, `{"id": 1, "tag_name": "v1", "html_url": "https://h/up/v1", "assets": [
				{"id": 11, "name": "a.zip", "size": 4, "content_type": "application/zip", "digest": "sha256:ab"},
				{"id": 12, "name": "b.exe", "size": 2, "digest": null}]}`)
		case "GET /repos/up/app/releases/tags/v2":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "Not Found"}`))
		case "GET /repos/up/app/releases/assets/11":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("download Accept = %q", r.Header.Get("Accept"))
			}
			http.Redirect(w, r, strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)+"/11", http.StatusFound)
		case "GET /repos/me/app/releases":
			if r.URL.Query().Get("page") == "1" {
				var rs []string
				for i := range 100 {
					rs = append(rs, fmt.Sprintf(`{"id": %d, "tag_name": "old%d"}`, 100+i, i))
				}
				fmt.Fprintf(w, "[%s]", strings.Join(rs, ","))
				return
			}
			w.Write([]byte(`[{"id": 7, "tag_name": "v1", "draft": true, "upload_url": "` + srv.URL + `/up/7/assets{?name,label}"}]`))
		case "POST /repos/me/app/releases":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			created = append(created, in)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id": 8, "tag_name": "v3", "upload_url": "` + srv.URL + `/up/8/assets{?name,label}"}`))
		case "POST /up/7/assets":
			body, _ := io.ReadAll(r.Body)
			if r.URL.Query().Get("name") != "a b.zip" || r.ContentLength != 4 || string(body) != "data" ||
				r.Header.Get("Content-Type") != "application/zip" {
				t.Errorf("upload: name=%q len=%d ct=%q body=%q", r.URL.Query().Get("name"), r.ContentLength,
					r.Header.Get("Content-Type"), body)
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id": 70, "name": "a b.zip", "size": 4, "digest": "sha256:cd"}`))
		case "GET /repos/me/app/releases/assets/70":
			w.Write([]byte(`{"id": 70, "name": "a b.zip", "size": 4, "digest": "sha256:cd"}`))
		case "DELETE /repos/me/app/releases/assets/70":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	f := NewReleaseFiles(srv.URL, staticToken("tok"), nil)
	ctx := context.Background()
	up, me := provider.Repo{Owner: "up", Name: "app"}, provider.Repo{Owner: "me", Name: "app"}

	rel, err := f.UpstreamRelease(ctx, up, "v1")
	if err != nil || len(rel.Assets) != 2 || rel.Assets[0].Digest != "sha256:ab" || rel.Assets[1].Digest != "" || rel.URL != "https://h/up/v1" {
		t.Fatalf("UpstreamRelease = %+v, %v", rel, err)
	}
	if _, err := f.UpstreamRelease(ctx, up, "v2"); !errors.Is(err, provider.ErrRefNotFound) {
		t.Fatalf("no release: %v", err)
	}
	body, err := f.DownloadAsset(ctx, up, 11)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "data" {
		t.Fatalf("download = %q", data)
	}

	fr, err := f.ForkRelease(ctx, me, "v1")
	if err != nil || fr == nil || fr.ID != 7 || !fr.Draft || fr.UploadURL != srv.URL+"/up/7/assets" {
		t.Fatalf("ForkRelease = %+v, %v", fr, err)
	}
	a, err := f.UploadAsset(ctx, me, fr, "a b.zip", "application/zip", 4, strings.NewReader("data"))
	if err != nil || a.ID != 70 || a.Digest != "sha256:cd" {
		t.Fatalf("UploadAsset = %+v, %v", a, err)
	}
	if got, err := f.GetAsset(ctx, me, 70); err != nil || got.Size != 4 {
		t.Fatalf("GetAsset = %+v, %v", got, err)
	}
	if err := f.DeleteAsset(ctx, me, 70); err != nil {
		t.Fatal(err)
	}

	f.CreateRelease(ctx, me, provider.ReleaseInput{TagName: "v3", Commit: "abc", Draft: true})
	f.CreateRelease(ctx, me, provider.ReleaseInput{TagName: "v3", Commit: "abc"})
	if len(created) != 2 || created[0]["draft"] != true || created[0]["target_commitish"] != nil ||
		created[1]["target_commitish"] != "abc" || created[1]["make_latest"] != "false" {
		t.Fatalf("created = %+v", created)
	}
}
