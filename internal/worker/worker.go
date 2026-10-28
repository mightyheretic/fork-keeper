// Package worker drains the webhook delivery queue in the background.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
)

// Processor handles one event. Returning an error retries the event with
// backoff, unless it is wrapped with Permanent.
type Processor func(ctx context.Context, ev provider.Event) error

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying.
func Permanent(err error) error { return permanentError{err} }

// Queue is the subset of the store the worker uses.
type Queue interface {
	ClaimNext(ctx context.Context) (store.Job, bool, error)
	Complete(ctx context.Context, id int64) error
	Retry(ctx context.Context, id int64, cause error, delay time.Duration) error
	Fail(ctx context.Context, id int64, cause error) error
}

// Worker processes queued deliveries one at a time.
type Worker struct {
	queue       Queue
	process     Processor
	log         *slog.Logger
	wake        chan struct{}
	PollEvery   time.Duration
	MaxAttempts int
}

// New returns a worker with default polling and retry settings.
func New(queue Queue, process Processor, log *slog.Logger) *Worker {
	return &Worker{
		queue:       queue,
		process:     process,
		log:         log,
		wake:        make(chan struct{}, 1),
		PollEvery:   5 * time.Second,
		MaxAttempts: 8,
	}
}

// Wake prompts the worker to check the queue now. It never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes deliveries until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.PollEvery)
	defer ticker.Stop()
	for {
		w.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.wake:
		}
	}
}

// drain processes due deliveries until none are left or ctx is cancelled.
func (w *Worker) drain(ctx context.Context) {
	for ctx.Err() == nil {
		job, ok, err := w.queue.ClaimNext(ctx)
		if err != nil {
			w.log.Error("claim delivery", "err", err)
			return
		}
		if !ok {
			return
		}
		w.handle(ctx, job)
	}
}

func (w *Worker) handle(ctx context.Context, job store.Job) {
	log := w.log.With("job", job.ID, "delivery", job.Event.DeliveryID,
		"kind", job.Event.Kind, "repo", job.Event.Repo.FullName(), "attempt", job.Attempts)

	err := w.process(ctx, job.Event)
	// Record the outcome even if ctx was cancelled mid-job, so the row
	// doesn't stay stuck in processing until the next restart.
	recordCtx := context.WithoutCancel(ctx)

	var perm permanentError
	switch {
	case err == nil:
		if err := w.queue.Complete(recordCtx, job.ID); err != nil {
			log.Error("mark delivery done", "err", err)
		}
	case errors.As(err, &perm) || job.Attempts >= w.MaxAttempts:
		log.Error("delivery failed permanently", "err", err)
		if err := w.queue.Fail(recordCtx, job.ID, err); err != nil {
			log.Error("mark delivery failed", "err", err)
		}
	default:
		delay := Backoff(job.Attempts)
		log.Warn("delivery failed, will retry", "err", err, "retry_in", delay)
		if err := w.queue.Retry(recordCtx, job.ID, err, delay); err != nil {
			log.Error("requeue delivery", "err", err)
		}
	}
}

// Backoff returns the delay before retry number attempt: 30s, 1m, 2m, ... capped at 1h.
func Backoff(attempt int) time.Duration {
	const base, max = 30 * time.Second, time.Hour
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}
