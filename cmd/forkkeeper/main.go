// Command forkkeeper runs the Fork Keeper service.
//
// This is a placeholder entrypoint so the container builds and passes its
// healthcheck. The real webhook handling, API and engines come with the V1 milestones.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Public listener: webhooks only.
	webhooks := http.NewServeMux()
	webhooks.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	webhooks.HandleFunc("POST /webhooks/github", func(w http.ResponseWriter, r *http.Request) {
		// TODO(milestone 1): verify X-Hub-Signature-256, dedupe on X-GitHub-Delivery, enqueue.
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})

	// Private listener: API for the CLI and the V2 UI.
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version)
	})

	servers := []*http.Server{
		{Addr: getenv("FORKKEEPER_WEBHOOK_ADDR", ":8080"), Handler: webhooks, ReadHeaderTimeout: 5 * time.Second},
		{Addr: getenv("FORKKEEPER_API_ADDR", ":8081"), Handler: api, ReadHeaderTimeout: 5 * time.Second},
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			slog.Info("listening", "addr", s.Addr, "version", version)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return nil
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
