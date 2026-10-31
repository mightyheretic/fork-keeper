package releasefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// MaxFileBytes is the largest file copied: GitHub's limit per release file.
const MaxFileBytes = 2 << 30

// Store records copied files.
type Store interface {
	SaveReleaseFile(ctx context.Context, f store.ReleaseFile) error
}

// Copier copies release files from upstream releases to the fork's.
type Copier struct {
	hosts  map[string]provider.ReleaseHost
	store  Store
	tmpDir string
	log    *slog.Logger
}

// New returns a Copier that downloads into tmpDir. Each file is deleted as
// soon as its upload is verified, or has failed.
func New(hosts map[string]provider.ReleaseHost, st Store, tmpDir string, log *slog.Logger) *Copier {
	removeStale(tmpDir, 24*time.Hour)
	return &Copier{hosts: hosts, store: st, tmpDir: tmpDir, log: log}
}

// removeStale deletes temporary files a crash left behind. Files younger
// than maxAge may belong to a copy still running in another process.
func removeStale(dir string, maxAge time.Duration) {
	paths, _ := filepath.Glob(filepath.Join(dir, "release-file-*"))
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) > maxAge {
			os.Remove(p)
		}
	}
}

// Request copies the files of one upstream release.
type Request struct {
	Provider string
	Upstream provider.Repo
	Fork     provider.Repo
	Tag      string
	// Commit is the tag's commit, where a published release's tag is
	// created on the fork.
	Commit string
	// Public publishes the fork's release; otherwise it stays a draft that
	// only people with write access to the fork can see.
	Public bool
}

// Result is what Copy did.
type Result struct {
	Tag string
	// ReleaseURL is the fork's release ("" if none was needed).
	ReleaseURL string
	Uploaded   []string
	Existing   []string
	Failed     []FileError
	// Note says why nothing was copied, if so.
	Note string
}

// FileError is a file that couldn't be copied.
type FileError struct {
	Name string
	Err  error
}

// Copy copies the Windows and Linux files that Select picks from the
// upstream release to a release of the same tag on the fork, creating it
// if needed. Files already there are left alone. Per-file failures are in
// Result and the store; the error is for failures that stop the copy.
func (c *Copier) Copy(ctx context.Context, req Request) (Result, error) {
	res := Result{Tag: req.Tag}
	host, ok := c.hosts[req.Provider]
	if !ok {
		return res, fmt.Errorf("no release host for provider %q", req.Provider)
	}
	up, err := host.UpstreamRelease(ctx, req.Upstream, req.Tag)
	if errors.Is(err, provider.ErrRefNotFound) {
		res.Note = "no release for this tag"
		return res, nil
	}
	if err != nil {
		return res, err
	}
	sel := Select(up.Assets)
	if len(sel) == 0 {
		res.Note = "no Windows or Linux files"
		return res, nil
	}

	rel, err := host.ForkRelease(ctx, req.Fork, req.Tag)
	if err != nil {
		return res, err
	}
	if rel == nil {
		rel, err = host.CreateRelease(ctx, req.Fork, provider.ReleaseInput{
			TagName: req.Tag, Commit: req.Commit, Draft: !req.Public,
			Name: fmt.Sprintf("%s (upstream files)", req.Tag),
			Body: fmt.Sprintf("Windows and Linux files of [%s %s](%s), copied by Fork Keeper so they stay available "+
				"if upstream removes them or changes its license.", req.Upstream.FullName(), req.Tag, up.URL),
		})
		if err != nil {
			return res, fmt.Errorf("create release %s on %s: %w", req.Tag, req.Fork.FullName(), err)
		}
	}
	res.ReleaseURL = rel.URL
	existing := map[string]provider.ReleaseAsset{}
	for _, a := range rel.Assets {
		existing[a.Name] = a
	}

	for _, s := range sel {
		rec := store.ReleaseFile{Provider: req.Provider, Repo: req.Upstream.FullName(), Tag: req.Tag, Name: s.Name,
			Platform: s.Platform, Size: s.Size, Fork: req.Fork.FullName(), ForkReleaseID: rel.ID}
		fail := func(err error) {
			res.Failed = append(res.Failed, FileError{Name: s.Name, Err: err})
			rec.Status, rec.Error = store.ReleaseFileFailed, err.Error()
			c.log.Warn("could not copy release file", "repo", rec.Repo, "tag", req.Tag, "file", s.Name, "err", err)
		}
		switch ex, ok := existing[s.Name]; {
		case ok && ex.Size == s.Size:
			res.Existing = append(res.Existing, s.Name)
			rec.Status, rec.ForkAssetID, rec.SHA256 = store.ReleaseFileUploaded, ex.ID, hexDigest(ex.Digest)
		case ok:
			fail(fmt.Errorf("a different %s (%d bytes, upstream's is %d) is already on the fork's release", s.Name, ex.Size, s.Size))
		case s.Size > MaxFileBytes:
			fail(fmt.Errorf("%d bytes is over GitHub's 2 GiB limit per release file", s.Size))
		default:
			asset, sum, err := c.copyOne(ctx, host, req, rel, s.ReleaseAsset)
			if err != nil {
				fail(err)
				break
			}
			res.Uploaded = append(res.Uploaded, s.Name)
			rec.Status, rec.ForkAssetID, rec.SHA256 = store.ReleaseFileUploaded, asset.ID, sum
		}
		if err := c.store.SaveReleaseFile(ctx, rec); err != nil {
			return res, err
		}
	}
	return res, nil
}

// copyOne downloads one file to a temporary file, uploads it and checks
// that the fork has exactly those bytes. The temporary file is always
// removed before returning.
func (c *Copier) copyOne(ctx context.Context, host provider.ReleaseHost, req Request, rel *provider.Release,
	a provider.ReleaseAsset) (provider.ReleaseAsset, string, error) {
	if err := os.MkdirAll(c.tmpDir, 0o700); err != nil {
		return provider.ReleaseAsset{}, "", err
	}
	tmp, err := os.CreateTemp(c.tmpDir, "release-file-*")
	if err != nil {
		return provider.ReleaseAsset{}, "", err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	body, err := host.DownloadAsset(ctx, req.Upstream, a.ID)
	if err != nil {
		return provider.ReleaseAsset{}, "", fmt.Errorf("download: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	body.Close()
	if err != nil {
		return provider.ReleaseAsset{}, "", fmt.Errorf("download: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if n != a.Size {
		return provider.ReleaseAsset{}, "", fmt.Errorf("download: got %d bytes, upstream lists %d", n, a.Size)
	}
	if want := hexDigest(a.Digest); want != "" && want != sum {
		return provider.ReleaseAsset{}, "", fmt.Errorf("download: SHA-256 %s doesn't match upstream's %s", sum, want)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return provider.ReleaseAsset{}, "", err
	}

	up, err := host.UploadAsset(ctx, req.Fork, rel, a.Name, a.ContentType, n, tmp)
	if err != nil {
		return provider.ReleaseAsset{}, "", fmt.Errorf("upload: %w", err)
	}
	if err := c.verify(ctx, host, req.Fork, up.ID, n, sum); err != nil {
		if derr := host.DeleteAsset(ctx, req.Fork, up.ID); derr != nil {
			err = fmt.Errorf("%w (and removing the bad upload failed: %v)", err, derr)
		}
		return provider.ReleaseAsset{}, "", fmt.Errorf("verify upload: %w", err)
	}
	return up, sum, nil
}

// verify checks the uploaded file's size and SHA-256, using the digest
// GitHub reports or, without one, by downloading it back.
func (c *Copier) verify(ctx context.Context, host provider.ReleaseHost, fork provider.Repo, id, size int64, sum string) error {
	got, err := host.GetAsset(ctx, fork, id)
	if err != nil {
		return err
	}
	if got.Size != size {
		return fmt.Errorf("fork has %d bytes, uploaded %d", got.Size, size)
	}
	if d := hexDigest(got.Digest); d != "" {
		if d != sum {
			return fmt.Errorf("fork reports SHA-256 %s, uploaded %s", d, sum)
		}
		return nil
	}
	body, err := host.DownloadAsset(ctx, fork, id)
	if err != nil {
		return err
	}
	defer body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return err
	}
	if d := hex.EncodeToString(h.Sum(nil)); d != sum {
		return fmt.Errorf("fork's copy has SHA-256 %s, uploaded %s", d, sum)
	}
	return nil
}

// hexDigest returns the hex of a "sha256:<hex>" digest, or "".
func hexDigest(d string) string {
	h, _ := strings.CutPrefix(d, "sha256:")
	if h == d {
		return ""
	}
	return h
}
