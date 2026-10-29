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
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/archive"
	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/gate"
	"github.com/mightyheretic/fork-keeper/internal/git"
	"github.com/mightyheretic/fork-keeper/internal/mainwatch"
	"github.com/mightyheretic/fork-keeper/internal/merge"
	"github.com/mightyheretic/fork-keeper/internal/pipeline"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
	"github.com/mightyheretic/fork-keeper/internal/rules"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/swh"
	"github.com/mightyheretic/fork-keeper/internal/watcher"
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
	case "poll":
		os.Exit(pollCmd(os.Args[2:]))
	case "license":
		os.Exit(licenseCmd(os.Args[2:]))
	case "rules":
		os.Exit(rulesCmd(os.Args[2:]))
	case "status":
		os.Exit(statusCmd(os.Args[2:]))
	case "unfreeze":
		os.Exit(unfreezeCmd(os.Args[2:]))
	case "sync":
		os.Exit(syncCmd(os.Args[2:]))
	case "fork":
		os.Exit(forkCmd(os.Args[2:]))
	case "archive":
		os.Exit(archiveCmd(os.Args[2:]))
	case "main":
		os.Exit(mainCmd(os.Args[2:]))
	case "healthcheck":
		os.Exit(healthcheck())
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve, check, poll, license, rules, status, unfreeze, sync, fork, archive, main, healthcheck or version)\n", cmd)
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
		log.Warn("no GitHub auth profiles configured; upstreams are polled anonymously and forks cannot be accessed")
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

// pollTargets builds the poll list from the configured repos.
func pollTargets(cfg *config.Config) []watcher.Target {
	targets := make([]watcher.Target, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		repo, err := provider.ParseRepo(r.Upstream) // validated in config
		if err != nil {
			continue
		}
		kind := provider.KindRelease
		if r.Watch == config.WatchTags {
			kind = provider.KindTag
		}
		targets = append(targets, watcher.Target{
			Provider:           github.Name,
			Repo:               repo,
			Kind:               kind,
			IncludePrereleases: cfg.WantsPrereleases(r),
		})
	}
	return targets
}

// newPoller wires the GitHub ref lister to the configured poll auth.
// upstreamToken returns tokens for reading upstream repos, using the poll
// auth profile (or anonymous access).
func upstreamToken(cfg *config.Config, auth *github.Auth) github.TokenFunc {
	return func(ctx context.Context, repo provider.Repo) (credentials.Secret, error) {
		name := cfg.PollAuthProfile(repo.FullName())
		if name == "" {
			return credentials.Secret{}, nil // anonymous
		}
		src := auth.Profile(name)
		if src == nil {
			return credentials.Secret{}, fmt.Errorf("auth profile %q does not exist", name)
		}
		return src.Token(ctx, repo)
	}
}

// repoToken returns tokens for any repo: forks use their own auth profile
// (github.repo_auth), everything else the upstream one.
func repoToken(cfg *config.Config, auth *github.Auth) github.TokenFunc {
	upstream := upstreamToken(cfg, auth)
	return func(ctx context.Context, repo provider.Repo) (credentials.Secret, error) {
		if cfg.IsFork(repo.FullName()) {
			return auth.Token(ctx, repo)
		}
		return upstream(ctx, repo)
	}
}

// newMergeEngine returns the merge engine and the fork hosts it uses. Git
// data lives under <data_dir>/git.
func newMergeEngine(cfg *config.Config, auth *github.Auth, st merge.Store, log *slog.Logger) (*merge.Engine, map[string]provider.ForkHost, error) {
	hosts := map[string]provider.ForkHost{github.Name: github.NewForks(cfg.GitHub.APIURL, repoToken(cfg, auth), nil)}
	runner, err := newGitRunner(cfg)
	if err != nil {
		return nil, nil, err
	}
	return merge.New(hosts, st, runner, filepath.Join(cfg.DataDir, "git"), log), hosts, nil
}

func newGitRunner(cfg *config.Config) (*git.Runner, error) {
	home := filepath.Join(cfg.DataDir, "git", "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create git dir: %w", err)
	}
	return &git.Runner{Home: home, Identity: git.Identity{Name: cfg.Merge.CommitterName, Email: cfg.Merge.CommitterEmail}}, nil
}

// newArchiver returns the source backup archiver. Backups live under
// <data_dir>/archive.
func newArchiver(cfg *config.Config, auth *github.Auth, creds credentials.Provider, st archive.Store, log *slog.Logger) (*archive.Archiver, error) {
	runner, err := newGitRunner(cfg)
	if err != nil {
		return nil, err
	}
	hosts := map[string]provider.GitRemote{github.Name: github.NewForks(cfg.GitHub.APIURL, repoToken(cfg, auth), nil)}
	var token func(context.Context) (credentials.Secret, error)
	if name := cfg.Archive.SoftwareHeritageToken; name != "" {
		token = func(ctx context.Context) (credentials.Secret, error) { return creds.Get(ctx, name) }
	}
	saver := swh.New(cfg.Archive.SoftwareHeritageURL, token, nil)
	return archive.New(hosts, st, runner, filepath.Join(cfg.DataDir, "archive"), creds, saver, log), nil
}

// mainTargets lists the upstreams whose default branch is watched.
func mainTargets(cfg *config.Config) []mainwatch.Target {
	var out []mainwatch.Target
	for _, r := range cfg.Repos {
		if !cfg.WatchesMainBranch(r) {
			continue
		}
		repo, err := provider.ParseRepo(r.Upstream)
		if err != nil {
			continue
		}
		kind := provider.KindRelease
		if r.Watch == config.WatchTags {
			kind = provider.KindTag
		}
		out = append(out, mainwatch.Target{Provider: github.Name, Repo: repo, Kind: kind, Config: r})
	}
	return out
}

func newMainWatcher(cfg *config.Config, auth *github.Auth, st mainwatch.Store, reports mainwatch.Reporter,
	archiver mainwatch.Archiver, log *slog.Logger) *mainwatch.Watcher {
	readers := map[string]provider.BranchReader{github.Name: github.NewRefs(cfg.GitHub.APIURL, upstreamToken(cfg, auth), nil)}
	w := mainwatch.New(cfg, readers, st, reports, rules.New(cfg), archiver, mainTargets(cfg), log)
	w.Interval = cfg.Watcher.PollInterval
	return w
}

func hasForks(cfg *config.Config) bool {
	for _, r := range cfg.Repos {
		if r.Fork != "" {
			return true
		}
	}
	return false
}

func newPoller(cfg *config.Config, auth *github.Auth, st watcher.Store, log *slog.Logger) *watcher.Poller {
	listers := map[string]provider.RefLister{github.Name: github.NewRefs(cfg.GitHub.APIURL, upstreamToken(cfg, auth), nil)}
	p := watcher.New(st, listers, pollTargets(cfg), log)
	p.Interval = cfg.Watcher.PollInterval
	return p
}

// warnAnonymousQuota warns when anonymous polling would exceed GitHub's
// 60 requests/hour limit for unauthenticated clients.
func warnAnonymousQuota(cfg *config.Config, log *slog.Logger) {
	perHour := 0.0
	polls := float64(time.Hour) / float64(cfg.Watcher.PollInterval)
	for _, r := range cfg.Repos {
		if cfg.PollAuthProfile(r.Upstream) != "" {
			continue
		}
		reqs := 1.0
		if r.Watch == config.WatchTags {
			reqs = 3 // up to maxTagPages
		}
		if cfg.WatchesMainBranch(r) {
			reqs += 2 // head, plus the root listing when it moved
		}
		perHour += reqs * polls
	}
	if perHour > 0 {
		log.Info("polling some upstreams anonymously", "requests_per_hour", int(perHour))
	}
	if perHour > 50 {
		log.Warn("anonymous polling may exceed GitHub's 60 requests/hour limit; configure a PAT and set watcher.auth, or raise watcher.poll_interval")
	}
}

// checkForks confirms git is installed and every fork is reachable with
// its auth profile.
func checkForks(ctx context.Context, log *slog.Logger, cfg *config.Config, auth *github.Auth) bool {
	if !hasForks(cfg) {
		return true
	}
	ok := true
	if _, err := exec.LookPath("git"); err != nil {
		log.Error("git is not installed; forks can't be synced", "err", err)
		ok = false
	}
	host := github.NewForks(cfg.GitHub.APIURL, repoToken(cfg, auth), nil)
	for _, r := range cfg.Repos {
		if r.Fork == "" {
			continue
		}
		fork, _ := provider.ParseRepo(r.Fork)
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		branch, err := host.DefaultBranch(ctx, fork)
		cancel()
		if err != nil {
			log.Error("fork not reachable", "fork", r.Fork, "profile", cfg.GitHub.AuthProfileFor(r.Fork), "err", err)
			ok = false
			continue
		}
		stable := r.StableBranch
		if stable == "" {
			stable = branch
		}
		log.Info("fork reachable", "fork", r.Fork, "upstream", r.Upstream, "stable_branch", stable,
			"staging_branch", r.StagingBranch, "strategy", r.MergeStrategy)
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
	auth := newGitHubAuth(cfg, creds)
	ok = validateAuth(ctx, log, auth) && ok
	ok = checkForks(ctx, log, cfg, auth) && ok
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

	engine, hosts, err := newMergeEngine(cfg, auth, db, log.With("component", "merge"))
	if err != nil {
		return err
	}
	var syncer pipeline.Syncer
	if hasForks(cfg) {
		if _, err := exec.LookPath("git"); err != nil {
			log.Error("git is not installed; forks won't be synced", "err", err)
		} else {
			syncer = engine
		}
	}
	licenseGate := gate.New(db, map[string]provider.ArchiveFetcher{
		github.Name: github.NewArchives(cfg.GitHub.APIURL, upstreamToken(cfg, auth)),
	}, log.With("component", "gate"))
	archiver, err := newArchiver(cfg, auth, creds, db, log.With("component", "archive"))
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("git"); err != nil {
		log.Error("git is not installed; source backups are off", "err", err)
		archiver = nil
	}
	pipe := pipeline.New(cfg, licenseGate, rules.New(cfg), db, syncer, log.With("component", "pipeline"))
	if archiver != nil {
		pipe.Archiver = archiver
	}
	w := worker.New(db, pipe.Process, log.With("component", "worker"))

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

	warnAnonymousQuota(cfg, log)
	poller := newPoller(cfg, auth, db, log.With("component", "watcher"))
	poller.OnQueued = w.Wake
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		poller.Run(ctx)
	}()

	var mainArchiver mainwatch.Archiver
	if archiver != nil {
		mainArchiver = archiver
	}
	mainWatcher := newMainWatcher(cfg, auth, db, licenseGate, mainArchiver, log.With("component", "mainwatch"))
	mainDone := make(chan struct{})
	go func() {
		defer close(mainDone)
		mainWatcher.Run(ctx)
	}()

	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		if syncer == nil {
			return
		}
		r := merge.NewReconciler(hosts, db, log.With("component", "reconciler"))
		r.Interval = cfg.Merge.CheckInterval
		r.AutoMerge, r.RequireCI = cfg.Merge.AutoMergeEnabled(), cfg.Merge.CIRequired()
		r.Run(ctx)
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
	<-pollerDone
	<-mainDone
	<-reconcilerDone
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

// healthcheck is used by the container HEALTHCHECK, so the image needs no curl.
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
