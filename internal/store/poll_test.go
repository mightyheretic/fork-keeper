package store

import (
	"context"
	"errors"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

var testKey = PollKey{Provider: "github", Repo: "up/app", Kind: provider.KindRelease}

func refEv(tag string, pre bool) provider.Event {
	return provider.Event{
		Provider:   "github",
		DeliveryID: "poll:release:up/app:" + tag,
		Kind:       provider.KindRelease,
		Repo:       provider.Repo{Owner: "up", Name: "app"},
		Tag:        tag,
		Prerelease: pre,
		Source:     provider.SourcePoll,
	}
}

func queueAll(provider.Event) bool { return true }

func TestRecordRefsBaselineThenNew(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	out, err := s.RecordRefs(ctx, testKey, `"e1"`, []provider.Event{refEv("v1", false), refEv("v2", false)}, queueAll)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Baseline || len(out.New) != 2 || len(out.Queued) != 0 {
		t.Fatalf("baseline outcome = %+v", out)
	}
	if _, ok, _ := s.ClaimNext(ctx); ok {
		t.Fatal("baseline queued work")
	}
	st, _ := s.PollState(ctx, testKey)
	if !st.Baselined || st.ETag != `"e1"` || st.LastSuccessAt.IsZero() {
		t.Fatalf("state = %+v", st)
	}

	out, err = s.RecordRefs(ctx, testKey, `"e2"`,
		[]provider.Event{refEv("v1", false), refEv("v2", false), refEv("v3", false)}, queueAll)
	if err != nil {
		t.Fatal(err)
	}
	if out.Baseline || len(out.New) != 1 || len(out.Queued) != 1 || out.Queued[0].Tag != "v3" {
		t.Fatalf("second outcome = %+v", out)
	}
	job, ok, err := s.ClaimNext(ctx)
	if err != nil || !ok || job.Event.Tag != "v3" || job.Event.Source != provider.SourcePoll {
		t.Fatalf("queued job = %+v ok=%v err=%v", job, ok, err)
	}

	// Seeing the same refs again changes nothing.
	out, _ = s.RecordRefs(ctx, testKey, `"e2"`, []provider.Event{refEv("v3", false)}, queueAll)
	if len(out.New) != 0 {
		t.Fatalf("repeat outcome = %+v", out)
	}
}

func TestRecordRefsFilter(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	s.RecordRefs(ctx, testKey, "", nil, queueAll) // baseline with no releases

	stableOnly := func(ev provider.Event) bool { return !ev.Prerelease }
	out, err := s.RecordRefs(ctx, testKey, "", []provider.Event{refEv("v2-rc1", true), refEv("v2", false)}, stableOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.New) != 2 || len(out.Queued) != 1 || out.Queued[0].Tag != "v2" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestPollStateTracking(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if st, err := s.PollState(ctx, testKey); err != nil || st.Baselined {
		t.Fatalf("unpolled state = %+v, %v", st, err)
	}
	if err := s.RecordPollError(ctx, testKey, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.PollState(ctx, testKey); st.LastError != "boom" || st.Baselined {
		t.Fatalf("after error = %+v", st)
	}
	s.RecordRefs(ctx, testKey, `"e"`, nil, queueAll)
	if err := s.RecordPollUnchanged(ctx, testKey); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.PollState(ctx, testKey); st.LastError != "" || st.ETag != `"e"` || !st.Baselined {
		t.Fatalf("after unchanged = %+v", st)
	}
}
