package mainwatch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/archive"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/notify"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

const mitText = `MIT License

Copyright (c) 2024 Example

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const buslText = "Business Source License 1.1\n\nLicensor: Example\nLicensed Work: app\n"

// fakeReader serves commits from memory: sha -> path -> content.
type fakeReader struct {
	head    string
	commits map[string]map[string]string
	reads   int
}

func blobID(content string) string {
	h := sha1.Sum([]byte(content))
	return hex.EncodeToString(h[:])
}

func (f *fakeReader) DefaultHead(_ context.Context, _ provider.Repo, etag string) (provider.BranchHead, error) {
	if etag == "etag-"+f.head {
		return provider.BranchHead{NotModified: true, ETag: etag}, nil
	}
	return provider.BranchHead{SHA: f.head, ETag: "etag-" + f.head}, nil
}

func (f *fakeReader) ListFiles(_ context.Context, _ provider.Repo, commit, dir string) ([]provider.TreeFile, error) {
	var out []provider.TreeFile
	dirs := map[string]bool{}
	for p, c := range f.commits[commit] {
		d, _ := path.Split(p)
		d = strings.TrimSuffix(d, "/")
		switch {
		case d == dir:
			out = append(out, provider.TreeFile{Path: p, ID: blobID(c), Size: int64(len(c))})
		case dir == "" && !dirs[strings.Split(d, "/")[0]]:
			dirs[strings.Split(d, "/")[0]] = true
			out = append(out, provider.TreeFile{Path: strings.Split(d, "/")[0] + "/"})
		}
	}
	return out, nil
}

func (f *fakeReader) ReadFile(_ context.Context, _ provider.Repo, id string) ([]byte, error) {
	f.reads++
	for _, files := range f.commits {
		for _, c := range files {
			if blobID(c) == id {
				return []byte(c), nil
			}
		}
	}
	return nil, provider.ErrRefNotFound
}

type fakeReports map[string]*license.Report

func (f fakeReports) Report(_ context.Context, _ string, _ provider.Repo, ref string) (*license.Report, error) {
	return f[ref], nil
}

type fakeArchiver struct {
	lastOpen string
	finds    [][]string
	backups  []archive.Request
}

func (f *fakeArchiver) FindLastOpen(_ context.Context, _ string, _ provider.Repo, _ string, bases, _ []string) (string, error) {
	f.finds = append(f.finds, bases)
	return f.lastOpen, nil
}

func (f *fakeArchiver) Backup(_ context.Context, req archive.Request) (archive.Result, error) {
	f.backups = append(f.backups, req)
	return archive.Result{}, nil
}

func scan(t *testing.T, files map[string]string) *license.Report {
	t.Helper()
	fs := license.Files{}
	for p, c := range files {
		fs[p] = []byte(c)
	}
	r, err := license.Scan(context.Background(), fs, license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func setup(t *testing.T, withRelease bool) (*Watcher, *fakeReader, *fakeArchiver, *store.Store, Target) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
credentials: {provider: env}
repos:
  - upstream: up/app
    fork: me/app
license:
  allowlist: [MIT]
  rules:
    - name: relicense
      from: [MIT]
      to: [BUSL-1.1]
      actions: [freeze, archive, notify]
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	repo := provider.Repo{Owner: "up", Name: "app"}
	release := map[string]string{"LICENSE": mitText, "main.go": "// SPDX-License-Identifier: MIT\npackage main\n"}
	if withRelease {
		_, err := st.RecordRefs(context.Background(), store.PollKey{Provider: "github", Repo: "up/app", Kind: provider.KindRelease}, "",
			[]provider.Event{{Provider: "github", Repo: repo, Tag: "v1.0.0", Kind: provider.KindRelease}},
			func(provider.Event) bool { return false })
		if err != nil {
			t.Fatal(err)
		}
	}
	reader := &fakeReader{commits: map[string]map[string]string{}}
	arch := &fakeArchiver{lastOpen: "open000000000000000000000000000000000000"}
	reports := fakeReports{"v1.0.0": scan(t, release)}
	w := New(cfg, map[string]provider.BranchReader{"github": reader}, st, reports, rules.New(cfg), arch, nil,
		slog.New(slog.DiscardHandler))
	r, _ := cfg.RepoFor("up/app")
	return w, reader, arch, st, Target{Provider: "github", Repo: repo, Kind: provider.KindRelease, Config: r}
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func TestEarlyWarningCatchesRelicenseBeforeRelease(t *testing.T) {
	w, reader, arch, st, target := setup(t, true)
	ctx := context.Background()

	// Same license as the release: nothing happens.
	reader.commits[sha('a')] = map[string]string{"LICENSE": mitText, "README.md": "hi", "src/x.go": "package x"}
	reader.head = sha('a')
	r := w.Check(ctx, target, false)
	if r.Err != nil || !r.Unchanged || r.BaseRef != "v1.0.0" || r.Diff != nil {
		t.Fatalf("first check: %+v", r)
	}

	// Head unchanged: answered by the ETag.
	if r = w.Check(ctx, target, false); r.Err != nil || !r.NotModified {
		t.Fatalf("unchanged head: %+v", r)
	}

	// New head, same license file: no file is read.
	reader.commits[sha('b')] = map[string]string{"LICENSE": mitText, "README.md": "changed"}
	reader.head = sha('b')
	reads := reader.reads
	if r = w.Check(ctx, target, false); r.Err != nil || !r.Unchanged || reader.reads != reads {
		t.Fatalf("same license file: %+v (reads %d -> %d)", r, reads, reader.reads)
	}

	// Relicense lands on the default branch.
	reader.commits[sha('c')] = map[string]string{"LICENSE": buslText}
	reader.head = sha('c')
	r = w.Check(ctx, target, false)
	if r.Err != nil || r.Diff == nil || !r.Diff.TopLevelChanged || r.Decision == nil || r.Decision.Rule != "relicense" {
		t.Fatalf("relicense: %+v", r)
	}
	if !r.NewlyFrozen || r.LastOpen != arch.lastOpen {
		t.Fatalf("relicense actions: %+v", r)
	}
	if len(arch.finds) != 1 || !slices.Equal(arch.finds[0], []string{sha('b'), "v1.0.0"}) {
		t.Fatalf("FindLastOpen bases: %v", arch.finds)
	}
	if len(arch.backups) != 1 || arch.backups[0].LastOpen != arch.lastOpen || arch.backups[0].LastGood != "v1.0.0" ||
		!arch.backups[0].Urgent || arch.backups[0].Fork != "me/app" {
		t.Fatalf("backups: %+v", arch.backups)
	}
	state, _ := st.RepoState(ctx, "github", "up/app")
	if !state.Frozen || state.FrozenRef != "v1.0.0" {
		t.Fatalf("state: %+v", state)
	}
	decisions, _ := st.Decisions(ctx, "up/app", 5)
	if len(decisions) != 1 || decisions[0].Ref != "main@"+sha('c')[:12] || slices.Contains(decisions[0].Pending, "archive") ||
		!slices.Contains(decisions[0].Pending, "notify") {
		t.Fatalf("decisions: %+v", decisions)
	}

	// Frozen repos aren't checked any more.
	if r = w.Check(ctx, target, false); r.Skipped == "" {
		t.Fatalf("frozen: %+v", r)
	}
}

func TestEarlyWarningWithoutReleaseRecordsBaseline(t *testing.T) {
	w, reader, arch, _, target := setup(t, false)
	ctx := context.Background()
	reader.commits[sha('a')] = map[string]string{"LICENSE": mitText}
	reader.head = sha('a')
	if r := w.Check(ctx, target, false); r.Err != nil || !r.Baseline {
		t.Fatalf("baseline: %+v", r)
	}
	// The next head is compared with the previous one.
	reader.commits[sha('b')] = map[string]string{"LICENSE": buslText}
	reader.head = sha('b')
	r := w.Check(ctx, target, false)
	if r.Err != nil || r.Diff == nil || r.BaseRef != "main@"+sha('a')[:12] {
		t.Fatalf("change: %+v", r)
	}
	// No release to fall back to: only the last-open commit is backed up.
	if len(arch.backups) != 1 || arch.backups[0].LastGood != "" || arch.backups[0].LastOpen == "" {
		t.Fatalf("backups: %+v", arch.backups)
	}
}

func TestEarlyWarningDryRunRecordsNothing(t *testing.T) {
	w, reader, arch, st, target := setup(t, true)
	ctx := context.Background()
	reader.commits[sha('c')] = map[string]string{"LICENSE": buslText}
	reader.head = sha('c')
	r := w.Check(ctx, target, true)
	if r.Err != nil || r.Diff == nil || r.Decision == nil || r.Decision.Rule != "relicense" {
		t.Fatalf("dry run: %+v", r)
	}
	state, _ := st.RepoState(ctx, "github", "up/app")
	mw, _ := st.MainWatch(ctx, "github", "up/app")
	if state.Frozen || mw.HeadSHA != "" || len(arch.backups) != 0 {
		t.Fatalf("dry run changed state: %+v %+v %d", state, mw, len(arch.backups))
	}
}

func TestLicensesDirIsRead(t *testing.T) {
	w, reader, _, _, target := setup(t, true)
	// A pointer at the root and the real license in LICENSES/, like
	// Elasticsearch: the LICENSES/ files are part of the top level.
	reader.commits[sha('a')] = map[string]string{"LICENSE.txt": "See LICENSES/ for the license terms.", "LICENSES/BUSL.txt": buslText}
	reader.head = sha('a')
	r := w.Check(context.Background(), target, true)
	if r.Err != nil || r.Diff == nil || !slices.Contains(r.Diff.Added, "BUSL-1.1") {
		t.Fatalf("LICENSES dir: %+v", r)
	}
}

type fakeNotifier struct{ n []notify.Notice }

func (f *fakeNotifier) Notify(_ context.Context, n notify.Notice) (bool, error) {
	f.n = append(f.n, n)
	return true, nil
}

func TestEarlyWarningNotifies(t *testing.T) {
	w, reader, _, _, target := setup(t, true)
	n := &fakeNotifier{}
	w.Notify = n
	reader.commits[sha('c')] = map[string]string{"LICENSE": buslText}
	reader.head = sha('c')
	if r := w.Check(context.Background(), target, false); r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(n.n) != 1 {
		t.Fatalf("notices = %+v", n.n)
	}
	m := n.n[0].Message
	if m.Priority != notify.PriorityUrgent || !strings.Contains(m.Title, "FROZEN") || !strings.Contains(m.Body, "frozen at v1.0.0") ||
		!strings.Contains(m.Body, "forkkeeper/last-open") || n.n[0].DecisionID == 0 || n.n[0].Key != "decision:github/up/app@main@"+sha('c')[:12] {
		t.Fatalf("notice = %+v", n.n[0])
	}
}
