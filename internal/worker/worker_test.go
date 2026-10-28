package worker

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

func setup(t *testing.T, process Processor, deliveries ...string) (*Worker, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, d := range deliveries {
		ev := provider.Event{Provider: "github", DeliveryID: d, Kind: provider.KindRelease,
			Repo: provider.Repo{Owner: "up", Name: "app"}, Tag: "v1", ReceivedAt: time.Now()}
		if _, err := s.EnqueueDelivery(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	return New(s, process, slog.New(slog.DiscardHandler)), s
}

func status(t *testing.T, s *store.Store, delivery string) (string, int) {
	t.Helper()
	st, attempts, err := s.DeliveryStatus(context.Background(), "github", delivery)
	if err != nil {
		t.Fatal(err)
	}
	return st, attempts
}

func TestDrainProcessesAll(t *testing.T) {
	var seen []string
	w, s := setup(t, func(_ context.Context, ev provider.Event) error {
		seen = append(seen, ev.DeliveryID)
		return nil
	}, "d1", "d2", "d3")

	w.drain(context.Background())

	if len(seen) != 3 || seen[0] != "d1" || seen[2] != "d3" {
		t.Fatalf("processed %v", seen)
	}
	for _, d := range seen {
		if st, _ := status(t, s, d); st != store.StatusDone {
			t.Fatalf("%s status = %q", d, st)
		}
	}
}

func TestTransientErrorRetries(t *testing.T) {
	w, s := setup(t, func(context.Context, provider.Event) error { return errors.New("network") }, "d1")
	w.drain(context.Background())
	// The retry is scheduled in the future, so one drain makes one attempt.
	if st, attempts := status(t, s, "d1"); st != store.StatusPending || attempts != 1 {
		t.Fatalf("status=%q attempts=%d", st, attempts)
	}
}

func TestPermanentErrorFails(t *testing.T) {
	w, s := setup(t, func(context.Context, provider.Event) error {
		return Permanent(errors.New("repo not configured"))
	}, "d1")
	w.drain(context.Background())
	if st, _ := status(t, s, "d1"); st != store.StatusFailed {
		t.Fatalf("status = %q", st)
	}
}

func TestMaxAttemptsFails(t *testing.T) {
	w, s := setup(t, func(context.Context, provider.Event) error { return errors.New("network") }, "d1")
	w.MaxAttempts = 1
	w.drain(context.Background())
	if st, _ := status(t, s, "d1"); st != store.StatusFailed {
		t.Fatalf("status = %q", st)
	}
}

func TestRunWakesAndStops(t *testing.T) {
	done := make(chan string, 1)
	w, _ := setup(t, func(_ context.Context, ev provider.Event) error {
		done <- ev.DeliveryID
		return nil
	}, "d1")
	w.PollEvery = time.Hour // only Wake or the initial drain can trigger work

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job not processed")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 20: time.Hour,
	} {
		if got := Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}
