// Package webhook receives webhook deliveries over HTTP, verifies them through
// the matching provider adapter, records them idempotently and hands them to
// the background queue. It acknowledges fast and never does the work inline.
package webhook

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// MaxBodyBytes matches GitHub's 25 MB cap on webhook payloads.
const MaxBodyBytes = 25 << 20

// Queue records a verified event for background processing. It reports false
// when the delivery was already recorded.
type Queue interface {
	EnqueueDelivery(ctx context.Context, ev provider.Event) (bool, error)
}

// Handler serves POST /webhooks/{provider}.
type Handler struct {
	parsers map[string]provider.WebhookParser
	queue   Queue
	// onEnqueue is called after a new delivery is recorded, to wake the worker.
	onEnqueue func()
	log       *slog.Logger
}

// NewHandler returns a handler for the given provider adapters. onEnqueue may be nil.
func NewHandler(queue Queue, onEnqueue func(), log *slog.Logger, parsers ...provider.WebhookParser) *Handler {
	h := &Handler{
		parsers:   make(map[string]provider.WebhookParser, len(parsers)),
		queue:     queue,
		onEnqueue: onEnqueue,
		log:       log,
	}
	for _, p := range parsers {
		h.parsers[p.Name()] = p
	}
	return h
}

// Register mounts the handler on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /webhooks/{provider}", h)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	parser, ok := h.parsers[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	log := h.log.With("provider", name)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	ev, err := parser.ParseWebhook(r.Header, body)
	switch {
	case errors.Is(err, provider.ErrUnauthenticated):
		log.Warn("rejected webhook with bad signature", "forwarded_for", r.Header.Get("X-Forwarded-For"))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	case errors.Is(err, provider.ErrMalformed):
		log.Warn("rejected malformed webhook", "err", err)
		http.Error(w, "malformed delivery", http.StatusBadRequest)
		return
	case err != nil:
		log.Error("webhook verification failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log = log.With("delivery", ev.DeliveryID)
	switch ev.Kind {
	case provider.KindPing:
		log.Info("webhook ping received")
		respond(w, http.StatusOK, "pong")
		return
	case provider.KindIgnored:
		respond(w, http.StatusAccepted, "ignored")
		return
	}

	inserted, err := h.queue.EnqueueDelivery(r.Context(), ev)
	if err != nil {
		// Let the provider retry: a 5xx makes GitHub mark the delivery failed
		// so it can be redelivered, and dedupe makes that safe.
		log.Error("enqueue webhook", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !inserted {
		log.Info("duplicate webhook delivery ignored")
		respond(w, http.StatusOK, "duplicate")
		return
	}
	log.Info("webhook queued", "kind", ev.Kind, "repo", ev.Repo.FullName(), "tag", ev.Tag)
	if h.onEnqueue != nil {
		h.onEnqueue()
	}
	respond(w, http.StatusAccepted, "queued")
}

func respond(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, msg+"\n")
}
