package gate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// fakeArchives serves an in-memory tarball per ref and counts downloads.
type fakeArchives struct {
	trees     map[string]map[string]string
	downloads map[string]int
}

func (f *fakeArchives) Archive(_ context.Context, _ provider.Repo, ref string) (io.ReadCloser, error) {
	tree, ok := f.trees[ref]
	if !ok {
		return nil, provider.ErrRefNotFound
	}
	f.downloads[ref]++
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range tree {
		tw.WriteHeader(&tar.Header{Name: "up-app-sha/" + name, Size: int64(len(body)), Typeflag: tar.TypeReg, Mode: 0o644})
		io.WriteString(tw, body)
	}
	tw.Close()
	zw.Close()
	return io.NopCloser(&buf), nil
}

func TestCheckBaselineThenDiff(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	arch := &fakeArchives{downloads: map[string]int{}, trees: map[string]map[string]string{
		"v1": {"package.json": `{"license":"MIT"}`, "a.go": "// SPDX-License-Identifier: MIT\n"},
		"v2": {"package.json": `{"license":"MIT"}`, "a.go": "// SPDX-License-Identifier: MIT\n"},
		"v3": {"package.json": `{"license":"BUSL-1.1"}`, "a.go": "// SPDX-License-Identifier: BUSL-1.1\n"},
	}}
	g := New(s, map[string]provider.ArchiveFetcher{"github": arch}, slog.New(slog.DiscardHandler))

	repo := provider.Repo{Owner: "up", Name: "app"}
	key := store.PollKey{Provider: "github", Repo: "up/app", Kind: provider.KindRelease}
	ev := func(tag string, day int) provider.Event {
		return provider.Event{Provider: "github", DeliveryID: "poll:" + tag, Kind: provider.KindRelease, Repo: repo,
			Tag: tag, PublishedAt: time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC)}
	}
	all := func(provider.Event) bool { return true }
	s.RecordRefs(ctx, key, "", []provider.Event{ev("v1", 1)}, all)

	// v1 is the first version: baseline, no diff.
	res, err := g.Check(ctx, ev("v1", 1))
	if err != nil || res.Diff != nil || res.Report.Declared != "MIT" {
		t.Fatalf("v1: res=%+v err=%v", res, err)
	}

	s.RecordRefs(ctx, key, "", []provider.Event{ev("v2", 2)}, all)
	res, err = g.Check(ctx, ev("v2", 2))
	if err != nil || res.Previous != "v1" || res.Diff == nil || res.Diff.Changed {
		t.Fatalf("v2: res=%+v err=%v", res, err)
	}

	s.RecordRefs(ctx, key, "", []provider.Event{ev("v3", 3)}, all)
	res, err = g.Check(ctx, ev("v3", 3))
	if err != nil || res.Previous != "v2" || !res.Diff.Changed || res.Diff.NewDeclared != "BUSL-1.1" {
		t.Fatalf("v3: res=%+v err=%v", res, err)
	}

	// Each version was downloaded once; later comparisons used the cache.
	for ref, n := range arch.downloads {
		if n != 1 {
			t.Errorf("%s downloaded %d times", ref, n)
		}
	}

	checks, _ := s.LicenseChecks(ctx, "up/app", 10)
	if len(checks) != 3 {
		t.Fatalf("recorded %d checks", len(checks))
	}

	if _, err := g.Check(ctx, ev("v9", 9)); err == nil {
		t.Fatal("missing ref should fail")
	}
}
