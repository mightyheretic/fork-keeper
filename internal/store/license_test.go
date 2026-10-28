package store

import (
	"context"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func TestPreviousRef(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	day := func(n int) time.Time { return time.Date(2026, 1, n, 0, 0, 0, 0, time.UTC) }
	rel := func(tag string, pre bool, published time.Time) provider.Event {
		ev := refEv(tag, pre)
		ev.PublishedAt = published
		return ev
	}
	// v1.0.1 is a backport, published after v2.0.0.
	s.RecordRefs(ctx, testKey, "", []provider.Event{
		rel("v1.0.0", false, day(1)),
		rel("v2.0.0", false, day(2)),
		rel("v2.1.0-rc1", true, day(3)),
		rel("v1.0.1", false, day(4)),
		rel("v2.1.0", false, day(5)),
	}, queueAll)

	key := func(ref string) RefKey { return RefKey{Provider: "github", Repo: "up/app", Ref: ref} }
	for ref, want := range map[string]string{
		"v2.1.0": "v2.0.0", // version order, skipping the rc and the backport
		"v1.0.1": "v1.0.0",
		"v2.0.0": "v1.0.1",
	} {
		got, ok, err := s.PreviousRef(ctx, key(ref), provider.KindRelease, time.Time{})
		if err != nil || !ok || got != want {
			t.Errorf("PreviousRef(%s) = %q, %v, %v; want %s", ref, got, ok, err, want)
		}
	}
	if _, ok, _ := s.PreviousRef(ctx, key("v1.0.0"), provider.KindRelease, time.Time{}); ok {
		t.Error("oldest release should have no previous ref")
	}
	// A ref not recorded yet (e.g. from a webhook) works too.
	if got, _, _ := s.PreviousRef(ctx, key("v3.0.0"), provider.KindRelease, day(6)); got != "v2.1.0" {
		t.Errorf("unrecorded ref: got %q", got)
	}
	// Names that aren't versions fall back to publish dates.
	s.RecordRefs(ctx, testKey, "", []provider.Event{rel("stable", false, day(7)), rel("latest", false, day(9))}, queueAll)
	if got, _, _ := s.PreviousRef(ctx, key("newest"), provider.KindRelease, day(10)); got != "latest" {
		t.Errorf("unversioned names: got %q", got)
	}
	// Tags have no dates and fall back to version order.
	tagKey := PollKey{Provider: "github", Repo: "up/app", Kind: provider.KindTag}
	tag := func(name string) provider.Event {
		ev := refEv(name, false)
		ev.Kind = provider.KindTag
		return ev
	}
	s.RecordRefs(ctx, tagKey, "", []provider.Event{tag("v1.9.0"), tag("v1.10.0"), tag("v1.2.0")}, queueAll)
	if got, _, _ := s.PreviousRef(ctx, key("v1.11.0"), provider.KindTag, time.Time{}); got != "v1.10.0" {
		t.Errorf("tag order: got %q", got)
	}
}

func TestLicenseReportsAndChecks(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	k := RefKey{Provider: "github", Repo: "up/app", Ref: "v1"}

	if _, ok, err := s.LicenseReport(ctx, k); ok || err != nil {
		t.Fatalf("empty cache: ok=%v err=%v", ok, err)
	}
	s.SaveLicenseReport(ctx, k, []byte(`{"declared":"MIT"}`))
	s.SaveLicenseReport(ctx, k, []byte(`{"declared":"ISC"}`))
	if b, ok, _ := s.LicenseReport(ctx, k); !ok || string(b) != `{"declared":"ISC"}` {
		t.Fatalf("cached report = %s", b)
	}

	s.SaveLicenseCheck(ctx, LicenseCheck{RefKey: k, Summary: "first"})
	k2 := k
	k2.Ref = "v2"
	s.SaveLicenseCheck(ctx, LicenseCheck{RefKey: k2, PreviousRef: "v1", Changed: true, Summary: "changed", Diff: []byte(`{}`)})
	checks, err := s.LicenseChecks(ctx, "up/app", 10)
	if err != nil || len(checks) != 2 {
		t.Fatalf("checks = %+v, %v", checks, err)
	}
	var got LicenseCheck
	for _, c := range checks {
		if c.Ref == "v2" {
			got = c
		}
	}
	if !got.Changed || got.PreviousRef != "v1" || string(got.Diff) != `{}` {
		t.Fatalf("v2 check = %+v", got)
	}
}
