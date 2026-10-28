package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testEvent(delivery string) provider.Event {
	return provider.Event{
		Provider:   "github",
		DeliveryID: delivery,
		Kind:       provider.KindRelease,
		Repo:       provider.Repo{Owner: "up", Name: "app"},
		Tag:        "v1.0.0",
		ReceivedAt: time.Now().UTC(),
	}
}

func TestEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	inserted, err := s.EnqueueDelivery(ctx, testEvent("d1"))
	if err != nil || !inserted {
		t.Fatalf("first enqueue: inserted=%v err=%v", inserted, err)
	}
	inserted, err = s.EnqueueDelivery(ctx, testEvent("d1"))
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue: inserted=%v err=%v", inserted, err)
	}

	// Same delivery ID from a different provider is a different delivery.
	other := testEvent("d1")
	other.Provider = "gitlab"
	if inserted, err := s.EnqueueDelivery(ctx, other); err != nil || !inserted {
		t.Fatalf("other provider: inserted=%v err=%v", inserted, err)
	}
}

func TestClaimCompleteFlow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, d := range []string{"d1", "d2"} {
		if _, err := s.EnqueueDelivery(ctx, testEvent(d)); err != nil {
			t.Fatal(err)
		}
	}

	job, ok, err := s.ClaimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if job.Event.DeliveryID != "d1" || job.Attempts != 1 || job.Event.Tag != "v1.0.0" {
		t.Fatalf("unexpected job: %+v", job)
	}
	if err := s.Complete(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// Completing twice is rejected: the row is no longer processing.
	if err := s.Complete(ctx, job.ID); err == nil {
		t.Fatal("expected error completing a finished job")
	}

	job2, ok, err := s.ClaimNext(ctx)
	if err != nil || !ok || job2.Event.DeliveryID != "d2" {
		t.Fatalf("second claim: job=%+v ok=%v err=%v", job2, ok, err)
	}
	if _, ok, _ := s.ClaimNext(ctx); ok {
		t.Fatal("claimed a job that is already processing")
	}

	if status, _, _ := s.DeliveryStatus(ctx, "github", "d1"); status != StatusDone {
		t.Fatalf("d1 status = %q", status)
	}
}

func TestRetryRespectsDelay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }

	s.EnqueueDelivery(ctx, testEvent("d1"))
	job, _, _ := s.ClaimNext(ctx)
	if err := s.Retry(ctx, job.ID, errors.New("transient"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ClaimNext(ctx); ok {
		t.Fatal("claimed a job before its retry delay")
	}

	now = now.Add(time.Minute)
	job, ok, err := s.ClaimNext(ctx)
	if err != nil || !ok || job.Attempts != 2 {
		t.Fatalf("claim after delay: job=%+v ok=%v err=%v", job, ok, err)
	}
	if err := s.Fail(ctx, job.ID, errors.New("permanent")); err != nil {
		t.Fatal(err)
	}
	if status, attempts, _ := s.DeliveryStatus(ctx, "github", "d1"); status != StatusFailed || attempts != 2 {
		t.Fatalf("status=%q attempts=%d", status, attempts)
	}
}

func TestRequeueStale(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	s.EnqueueDelivery(ctx, testEvent("d1"))
	if _, ok, _ := s.ClaimNext(ctx); !ok {
		t.Fatal("claim failed")
	}

	n, err := s.RequeueStale(ctx)
	if err != nil || n != 1 {
		t.Fatalf("requeue: n=%d err=%v", n, err)
	}
	if _, ok, _ := s.ClaimNext(ctx); !ok {
		t.Fatal("requeued job not claimable")
	}
}

func TestMigrationsAreReentrant(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		s.Close()
	}
}
