// Command forkkeeper runs the Fork Keeper service.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/webhook"
	"github.com/mightyheretic/fork-keeper/internal/worker"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	case "healthcheck":
		os.Exit(healthcheck())
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve, healthcheck or version)\n", cmd)
		os.Exit(2)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newLogger() *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(getenv("FORKKEEPER_LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// fileSecret reads a secret from $CREDENTIALS_DIRECTORY on every call, so a
// rotated file takes effect without a restart. Replaced by the pluggable
// credential provider in milestone 2.
func fileSecret(name string) github.SecretFunc {
	return func() ([]byte, error) {
		dir := os.Getenv("CREDENTIALS_DIRECTORY")
		if dir == "" {
			return nil, errors.New("CREDENTIALS_DIRECTORY is not set")
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read credential %q: %w", name, err)
		}
		return bytes.TrimRight(b, "\r\n"), nil
	}
}

func serve() error {
	log := newLogger()
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dataDir := getenv("FORKKEEPER_DATA_DIR", "/var/lib/forkkeeper")
	db, err := store.Open(ctx, filepath.Join(dataDir, "forkkeeper.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if n, err := db.RequeueStale(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("requeued deliveries interrupted by previous shutdown", "count", n)
	}

	// Milestone 1 only records and logs events. License checks and merges
	// plug in here in later milestones.
	w := worker.New(db, func(ctx context.Context, ev provider.Event) error {
		log.Info("event received", "kind", ev.Kind, "repo", ev.Repo.FullName(),
			"tag", ev.Tag, "prerelease", ev.Prerelease, "delivery", ev.DeliveryID)
		return nil
	}, log.With("component", "worker"))

	// Public listener: webhooks only.
	public := http.NewServeMux()
	public.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(rw, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	})
	webhook.NewHandler(db, w.Wake, log.With("component", "webhook"),
		github.NewWebhooks(fileSecret("github_webhook_secret")),
	).Register(public)

	// Private listener: API for the CLI and the V2 UI.
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/version", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(rw, version)
	})

	servers := []*http.Server{
		newServer(getenv("FORKKEEPER_WEBHOOK_ADDR", ":8080"), public),
		newServer(getenv("FORKKEEPER_API_ADDR", ":8081"), api),
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		w.Run(ctx)
	}()

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Info("listening", "addr", s.Addr, "version", version)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	var runErr error
	select {
	case runErr = <-errc:
		stop()
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	<-workerDone
	return runErr
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// healthcheck is used by the container HEALTHCHECK; distroless has no curl.
func healthcheck() int {
	_, port, err := net.SplitHostPort(getenv("FORKKEEPER_WEBHOOK_ADDR", ":8080"))
	if err != nil {
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
