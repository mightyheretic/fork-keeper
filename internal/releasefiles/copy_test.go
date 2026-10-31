package releasefiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

type fakeHost struct {
	upstream map[int64][]byte // asset ID -> content
	assets   []provider.ReleaseAsset
	fork     *provider.Release
	forkData map[int64][]byte
	created  []provider.ReleaseInput
	deleted  []int64
	nextID   int64
	// noDigest leaves Digest empty on fork assets, as older GitHub did.
	noDigest bool
	// corrupt flips a byte of every upload.
	corrupt bool
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (f *fakeHost) UpstreamRelease(_ context.Context, _ provider.Repo, tag string) (*provider.Release, error) {
	if tag != "v1" {
		return nil, provider.ErrRefNotFound
	}
	return &provider.Release{ID: 1, TagName: tag, URL: "https://example.com/up/v1", Assets: f.assets}, nil
}

func (f *fakeHost) DownloadAsset(_ context.Context, repo provider.Repo, id int64) (io.ReadCloser, error) {
	data := f.upstream[id]
	if repo.Owner == "me" {
		data = f.forkData[id]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeHost) ForkRelease(context.Context, provider.Repo, string) (*provider.Release, error) {
	return f.fork, nil
}

func (f *fakeHost) CreateRelease(_ context.Context, _ provider.Repo, in provider.ReleaseInput) (*provider.Release, error) {
	f.created = append(f.created, in)
	f.fork = &provider.Release{ID: 9, TagName: in.TagName, Draft: in.Draft, URL: "https://example.com/me/v1", UploadURL: "u"}
	return f.fork, nil
}

func (f *fakeHost) UploadAsset(_ context.Context, _ provider.Repo, rel *provider.Release, name, ct string, size int64, body io.Reader) (provider.ReleaseAsset, error) {
	data, _ := io.ReadAll(body)
	if f.corrupt {
		data[0] ^= 0xff
	}
	f.nextID++
	id := 100 + f.nextID
	if f.forkData == nil {
		f.forkData = map[int64][]byte{}
	}
	f.forkData[id] = data
	a := provider.ReleaseAsset{ID: id, Name: name, Size: int64(len(data)), ContentType: ct}
	if !f.noDigest {
		a.Digest = digest(data)
	}
	rel.Assets = append(rel.Assets, a)
	return a, nil
}

func (f *fakeHost) GetAsset(_ context.Context, _ provider.Repo, id int64) (provider.ReleaseAsset, error) {
	for _, a := range f.fork.Assets {
		if a.ID == id {
			return a, nil
		}
	}
	return provider.ReleaseAsset{}, provider.ErrRefNotFound
}

func (f *fakeHost) DeleteAsset(_ context.Context, _ provider.Repo, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func newFake(files map[string]string) *fakeHost {
	f := &fakeHost{upstream: map[int64][]byte{}}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for i, n := range names {
		data := []byte(files[n])
		id := int64(i + 1)
		f.upstream[id] = data
		f.assets = append(f.assets, provider.ReleaseAsset{ID: id, Name: n, Size: int64(len(data)), Digest: digest(data)})
	}
	return f
}

func setup(t *testing.T, host *fakeHost) (*Copier, *store.Store, string) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tmp := filepath.Join(t.TempDir(), "tmp")
	return New(map[string]provider.ReleaseHost{"github": host}, st, tmp, slog.New(slog.DiscardHandler)), st, tmp
}

var (
	up   = provider.Repo{Owner: "up", Name: "app"}
	fork = provider.Repo{Owner: "me", Name: "app"}
)

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestCopyCreatesDraftAndVerifies(t *testing.T) {
	ctx := context.Background()
	for _, noDigest := range []bool{false, true} {
		host := newFake(map[string]string{"app-linux-x86_64.tar.gz": "linux build", "app-win64.zip": "windows build",
			"app-darwin.tar.gz": "mac build", "app_amd64.deb": "deb"})
		host.noDigest = noDigest
		c, st, tmp := setup(t, host)
		res, err := c.Copy(ctx, Request{Provider: "github", Upstream: up, Fork: fork, Tag: "v1", Commit: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(res.Uploaded)
		if !slices.Equal(res.Uploaded, []string{"app-linux-x86_64.tar.gz", "app-win64.zip"}) || len(res.Failed) != 0 {
			t.Fatalf("noDigest=%v: %+v", noDigest, res)
		}
		if len(host.created) != 1 || !host.created[0].Draft || res.ReleaseURL != "https://example.com/me/v1" {
			t.Fatalf("release = %+v", host.created)
		}
		recs, _ := st.ReleaseFiles(ctx, "github", "up/app")
		if len(recs) != 2 || recs[0].Status != store.ReleaseFileUploaded || recs[0].SHA256 == "" || recs[0].ForkAssetID == 0 {
			t.Fatalf("records = %+v", recs)
		}
		emptyDir(t, tmp)

		// A second run finds the files on the fork and uploads nothing.
		res, err = c.Copy(ctx, Request{Provider: "github", Upstream: up, Fork: fork, Tag: "v1"})
		if err != nil || len(res.Uploaded) != 0 || len(res.Existing) != 2 || len(host.created) != 1 {
			t.Fatalf("second run: %+v %v", res, err)
		}
	}
}

func TestCopyRemovesBadUpload(t *testing.T) {
	ctx := context.Background()
	for _, noDigest := range []bool{false, true} {
		host := newFake(map[string]string{"app-linux-x86_64.tar.gz": "linux build"})
		host.corrupt, host.noDigest = true, noDigest
		c, st, tmp := setup(t, host)
		res, err := c.Copy(ctx, Request{Provider: "github", Upstream: up, Fork: fork, Tag: "v1"})
		if err != nil || len(res.Uploaded) != 0 || len(res.Failed) != 1 || len(host.deleted) != 1 {
			t.Fatalf("noDigest=%v: %+v %v deleted=%v", noDigest, res, err, host.deleted)
		}
		recs, _ := st.ReleaseFiles(ctx, "", "")
		if len(recs) != 1 || recs[0].Status != store.ReleaseFileFailed {
			t.Fatalf("records = %+v", recs)
		}
		emptyDir(t, tmp)
	}
}

func TestCopyNothingToDo(t *testing.T) {
	ctx := context.Background()
	host := newFake(map[string]string{"app-darwin.tar.gz": "mac"})
	c, _, _ := setup(t, host)
	if res, err := c.Copy(ctx, Request{Provider: "github", Upstream: up, Fork: fork, Tag: "v1"}); err != nil || res.Note == "" || len(host.created) != 0 {
		t.Fatalf("mac only: %+v %v", res, err)
	}
	if res, err := c.Copy(ctx, Request{Provider: "github", Upstream: up, Fork: fork, Tag: "v2"}); err != nil || res.Note != "no release for this tag" {
		t.Fatalf("no release: %+v %v", res, err)
	}
}

func TestCopyKeepsDifferentExistingFile(t *testing.T) {
	ctx := context.Background()
	host := newFake(map[string]string{"app-win64.zip": "windows build"})
	host.fork = &provider.Release{ID: 9, TagName: "v1", UploadURL: "u",
		Assets: []provider.ReleaseAsset{{ID: 50, Name: "app-win64.zip", Size: 3}}}
	c, _, _ := setup(t, host)
	res, err := c.Copy(ctx, Request{Provider: "github", Upstream: up, Fork: fork, Tag: "v1"})
	if err != nil || len(res.Failed) != 1 || len(host.deleted) != 0 || len(host.forkData) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}
