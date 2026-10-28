// Command forkkeeper runs the Fork Keeper service.
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
	"path/filepath"
	"syscall"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/webhook"
	"github.com/mightyheretic/fork-keeper/internal/worker"
)

var version = "dev"

// revalidateEvery re-checks credentials so an approaching expiry is noticed.
const revalidateEvery = 24 * time.Hour

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
	case "check":
		os.Exit(check())
	case "healthcheck":
		os.Exit(healthcheck())
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve, check, healthcheck or version)\n", cmd)
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

func loadConfig() (*config.Config, error) {
	return config.Load(getenv("FORKKEEPER_CONFIG", "/etc/forkkeeper/config.yaml"))
}

func newCredentials(c config.Credentials) credentials.Provider {
	switch c.Provider {
	case "env":
		return credentials.Env{Prefix: c.Env.Prefix}
	case "command":
		return &credentials.Command{Argv: c.Command.Argv, Timeout: c.Command.Timeout}
	default:
		return credentials.File{Dir: c.File.Dir}
	}
}

func newGitHubAuth(cfg *config.Config, creds credentials.Provider) *github.Auth {
	sources := make(map[string]github.TokenSource, len(cfg.GitHub.Auth))
	for name, a := range cfg.GitHub.Auth {
		switch a.Type {
		case config.AuthApp:
			sources[name] = github.NewAppAuth(github.AppConfig{
				APIURL:         cfg.GitHub.APIURL,
				AppID:          a.AppID,
				InstallationID: a.InstallationID,
				PrivateKey:     a.PrivateKey,
			}, creds)
		case config.AuthPAT:
			sources[name] = github.NewPATAuth(cfg.GitHub.APIURL, a.Token, creds, nil)
		}
	}
	return github.NewAuth(sources, cfg.GitHub.AuthProfileFor)
}

// auditCredentials checks that every referenced credential exists and is
// stored safely. It returns false if any is missing or unreadable.
func auditCredentials(ctx context.Context, log *slog.Logger, creds credentials.Provider, names []string) bool {
	ok := true
	for _, name := range names {
		if _, err := creds.Get(ctx, name); err != nil {
			log.Error("credential unavailable", "provider", creds.Kind(), "err", err)
			ok = false
			continue
		}
		if c, isChecker := creds.(credentials.Checker); isChecker {
			warnings, err := c.Check(name)
			if err != nil {
				log.Warn("credential check failed", "credential", name, "err", err)
			}
			for _, w := range warnings {
				log.Warn(w, "credential", name)
			}
		}
	}
	return ok
}

// validateAuth makes one GitHub call per auth profile and logs what each
// credential can do. It returns false if any profile failed.
func validateAuth(ctx context.Context, log *slog.Logger, auth *github.Auth) bool {
	profiles := auth.Profiles()
	if len(profiles) == 0 {
		log.Warn("no GitHub auth profiles configured; webhooks are received but repos cannot be accessed")
		return true
	}
	ok := true
	for _, name := range profiles {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r, err := auth.Profile(name).Validate(ctx)
		cancel()
		if err != nil {
			log.Error("GitHub credential validation failed", "profile", name, "err", err)
			ok = false
			continue
		}
		attrs := []any{"profile", name, "kind", r.Kind, "identity", r.Identity}
		if len(r.Permissions) > 0 {
			attrs = append(attrs, "permissions", r.Permissions)
		}
		if len(r.Events) > 0 {
			attrs = append(attrs, "events", r.Events)
		}
		if len(r.Scopes) > 0 {
			attrs = append(attrs, "scopes", r.Scopes)
		}
		if !r.ExpiresAt.IsZero() {
			attrs = append(attrs, "expires_at", r.ExpiresAt)
		}
		log.Info("GitHub credential valid", attrs...)
		for _, w := range r.Warnings {
			log.Warn(w, "profile", name)
		}
	}
	return ok
}

// check validates the config and credentials, then exits. Use it after
// setup: `docker compose run --rm forkkeeper check`.
func check() int {
	log := newLogger()
	cfg, err := loadConfig()
	if err != nil {
		log.Error("invalid config", "err", err)
		return 1
	}
	ctx := context.Background()
	creds := newCredentials(cfg.Credentials)
	ok := auditCredentials(ctx, log, creds, cfg.CredentialNames())
	ok = validateAuth(ctx, log, newGitHubAuth(cfg, creds)) && ok
	if !ok {
		return 1
	}
	log.Info("configuration and credentials OK")
	return 0
}

func serve() error {
	log := newLogger()
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	creds := newCredentials(cfg.Credentials)
	auth := newGitHubAuth(cfg, creds)
	// Report problems but keep running: webhooks are still recorded, and a
	// fixed credential is picked up on SIGHUP.
	auditCredentials(ctx, log, creds, cfg.CredentialNames())
	validateAuth(ctx, log, auth)

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	if n, err := db.RequeueStale(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("requeued deliveries interrupted by previous shutdown", "count", n)
	}

	// Later milestones replace this with license checks and merges.
	w := worker.New(db, func(ctx context.Context, ev provider.Event) error {
		log.Info("event received", "kind", ev.Kind, "repo", ev.Repo.FullName(),
			"tag", ev.Tag, "prerelease", ev.Prerelease, "delivery", ev.DeliveryID)
		return nil
	}, log.With("component", "worker"))

	webhookSecret := func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s, err := creds.Get(ctx, cfg.GitHub.WebhookSecret)
		return s.Bytes(), err
	}

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
		github.NewWebhooks(webhookSecret),
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
	go watchCredentials(ctx, log, cfg, creds, auth)

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

// watchCredentials reloads credentials on SIGHUP and re-validates them daily.
func watchCredentials(ctx context.Context, log *slog.Logger, cfg *config.Config, creds credentials.Provider, auth *github.Auth) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	ticker := time.NewTicker(revalidateEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			log.Info("SIGHUP received, reloading credentials")
			if r, ok := creds.(credentials.Reloader); ok {
				r.Reload()
			}
			auth.Reset()
			auditCredentials(ctx, log, creds, cfg.CredentialNames())
		case <-ticker.C:
		}
		validateAuth(ctx, log, auth)
	}
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
