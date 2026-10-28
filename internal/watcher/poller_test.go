package watcher

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// fakeLister serves a mutable release list with a version-based ETag.
type fakeLister struct {
	releases []provider.Ref // newest first
	version  int
	etags    []string // If-None-Match values received
	err      error
	calls    int
}

func (f *fakeLister) etag() string { return "e" + string(rune('0'+f.version)) }

func (f *fakeLister) ListReleases(_ context.Context, _ provider.Repo, etag string) (provider.RefList, error) {
	f.calls++
	f.etags = append(f.etags, etag)
	if f.err != nil {
		return provider.RefList{}, f.err
	}
	if etag != "" && etag == f.etag() {
		return provider.RefList{NotModified: true, ETag: etag}, nil
	}
	return provider.RefList{Refs: append([]provider.Ref(nil), f.releases...), ETag: f.etag()}, nil
}

func (f *fakeLister) ListTags(context.Context, provider.Repo) (provider.RefList, error) {
	f.calls++
	return provider.RefList{Refs: []provider.Ref{{Kind: provider.KindTag, Name: "t1"}}}, f.err
}

func (f *fakeLister) publish(tag string, pre bool) {
	f.releases = append([]provider.Ref{{Kind: provider.KindRelease, Name: tag, Prerelease: pre}}, f.releases...)
	f.version++
}

var target = Target{Provider: "github", Repo: provider.Repo{Owner: "up", Name: "app"}, Kind: provider.KindRelease}

func setup(t *testing.T, targets ...Target) (*Poller, *fakeLister, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fakeLister{}
	f.publish("v1.0.0", false)
	f.publish("v1.1.0", false)
	p := New(s, map[string]provider.RefLister{"github": f}, targets, slog.New(slog.DiscardHandler))
	p.Spacing = 0
	return p, f, s
}

func claimAll(t *testing.T, s *store.Store) []string {
	t.Helper()
	var tags []string
	for {
		job, ok, err := s.ClaimNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return tags
		}
		tags = append(tags, job.Event.Tag)
	}
}

func TestBaselineThenQueueOldestFirst(t *testing.T) {
	p, f, s := setup(t, target)
	ctx := context.Background()
	wakes := 0
	p.OnQueued = func() { wakes++ }

	r := p.PollOne(ctx, target, false)
	if r.Err != nil || !r.Outcome.Baseline || len(r.Outcome.New) != 2 {
		t.Fatalf("baseline = %+v", r)
	}
	if f.etags[0] != "" {
		t.Fatal("first poll should not send an ETag")
	}

	r = p.PollOne(ctx, target, false)
	if !r.NotModified || f.etags[1] != "e2" {
		t.Fatalf("second poll: notModified=%v etag sent=%q", r.NotModified, f.etags[1])
	}

	f.publish("v1.2.0", false)
	f.publish("v1.3.0-rc1", true)
	f.publish("v1.3.0", false)
	r = p.PollOne(ctx, target, false)
	if r.Err != nil || len(r.Outcome.New) != 3 || len(r.Outcome.Queued) != 2 {
		t.Fatalf("third poll = %+v", r.Outcome)
	}
	if got := claimAll(t, s); len(got) != 2 || got[0] != "v1.2.0" || got[1] != "v1.3.0" {
		t.Fatalf("queued %v, want [v1.2.0 v1.3.0] (oldest first, no pre-release)", got)
	}
	if wakes != 1 {
		t.Fatalf("OnQueued called %d times", wakes)
	}
}

func TestIncludePrereleases(t *testing.T) {
	pre := target
	pre.IncludePrereleases = true
	p, f, s := setup(t, pre)
	ctx := context.Background()
	p.PollOne(ctx, pre, false)
	f.publish("v2.0.0-rc1", true)
	p.PollOne(ctx, pre, false)
	if got := claimAll(t, s); len(got) != 1 || got[0] != "v2.0.0-rc1" {
		t.Fatalf("queued %v", got)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	f := &fakeLister{}
	f.publish("v1", false)
	// A nil store would panic if dry-run touched it.
	p := New(nil, map[string]provider.RefLister{"github": f}, nil, slog.New(slog.DiscardHandler))
	r := p.PollOne(context.Background(), target, true)
	if r.Err != nil || len(r.Refs) != 1 {
		t.Fatalf("dry run = %+v", r)
	}
}

func TestRateLimitPausesRemainingTargets(t *testing.T) {
	other := target
	other.Repo = provider.Repo{Owner: "up", Name: "other"}
	p, f, s := setup(t, target, other)
	f.err = &provider.RateLimitError{Reset: time.Now().Add(30 * time.Minute)}

	results := p.PollAll(context.Background())
	if len(results) != 2 || results[0].Err == nil || results[1].Err == nil {
		t.Fatalf("results = %+v", results)
	}
	if f.calls != 1 {
		t.Fatalf("lister called %d times; the second target should be skipped", f.calls)
	}
	st, _ := s.PollState(context.Background(), store.PollKey{Provider: "github", Repo: "up/app", Kind: provider.KindRelease})
	if st.LastError == "" {
		t.Fatal("poll error not recorded")
	}

	// After the reset time, polling resumes.
	f.err = nil
	p.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	results = p.PollAll(context.Background())
	if results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("after reset: %+v", results)
	}
}

func TestTagsWatch(t *testing.T) {
	tags := target
	tags.Kind = provider.KindTag
	p, _, _ := setup(t, tags)
	r := p.PollOne(context.Background(), tags, false)
	if r.Err != nil || !r.Outcome.Baseline || len(r.Outcome.New) != 1 || r.Outcome.New[0].Kind != provider.KindTag {
		t.Fatalf("tags poll = %+v", r)
	}
}
