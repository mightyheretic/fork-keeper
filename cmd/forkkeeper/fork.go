package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
)

const forkUsage = `usage: forkkeeper fork [--profile name] [--org org] <owner/repo>

Forks <owner/repo> on GitHub into the account of the auth profile's token
(or into --org), waits until GitHub has copied it, and prints the config
to add so Fork Keeper maintains it. Forking a repo you already forked
returns the existing fork.

  --profile  GitHub auth profile to use (default: watcher.auth, or the
             only profile configured)
  --org      create the fork in this organization instead`

// forkCmd creates a fork of an upstream repo.
func forkCmd(args []string) int {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, forkUsage) }
	profile := fs.String("profile", "", "")
	org := fs.String("org", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	upstream, err := provider.ParseRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		return 1
	}
	name, err := forkProfile(cfg, *profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	src := newGitHubAuth(cfg, newCredentials(cfg.Credentials)).Profile(name)
	if src == nil {
		fmt.Fprintf(os.Stderr, "auth profile %q does not exist\n", name)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	forks := github.NewForks(cfg.GitHub.APIURL, func(ctx context.Context, repo provider.Repo) (credentials.Secret, error) {
		return src.Token(ctx, repo)
	}, nil)
	fmt.Printf("Forking %s with auth profile %q...\n", upstream.FullName(), name)
	fork, branch, err := forks.CreateFork(ctx, upstream, *org)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fork failed:", err)
		var apiErr *github.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound) {
			fmt.Fprintln(os.Stderr, "A fine-grained PAT needs Administration: read & write and Contents: read, with\n"+
				"repository access set to \"All repositories\" (the fork doesn't exist yet, so it\n"+
				"can't be selected). A GitHub App can only fork into an organization it is installed on.")
		}
		return 1
	}
	fmt.Printf("Fork ready: https://github.com/%s (default branch %s)\n\n", fork.FullName(), branch)

	if r, ok := cfg.RepoFor(upstream.FullName()); ok && r.Fork != "" {
		if r.Fork == fork.FullName() {
			fmt.Println("Already in your config.")
		} else {
			fmt.Printf("Note: your config maps %s to a different fork, %s.\n", upstream.FullName(), r.Fork)
		}
	} else {
		fmt.Printf("Add it to your config so Fork Keeper maintains it:\n\nrepos:\n  - upstream: %s\n    fork: %s\n\n",
			upstream.FullName(), fork.FullName())
	}
	if cfg.GitHub.AuthProfileFor(fork.FullName()) != name {
		fmt.Printf("and route the fork to the %q profile so it can push and open PRs:\n\ngithub:\n  repo_auth:\n    \"%s/*\": %s\n",
			name, fork.Owner, name)
	}
	return 0
}

// forkProfile picks the auth profile used to create a fork.
func forkProfile(cfg *config.Config, flagValue string) (string, error) {
	switch {
	case flagValue != "":
		return flagValue, nil
	case cfg.Watcher.Auth != "" && cfg.Watcher.Auth != config.Anonymous:
		return cfg.Watcher.Auth, nil
	case len(cfg.GitHub.Auth) == 1:
		return slices.Collect(maps.Keys(cfg.GitHub.Auth))[0], nil
	case len(cfg.GitHub.Auth) == 0:
		return "", errors.New("no GitHub auth profiles configured; forking needs a token")
	}
	return "", fmt.Errorf("several auth profiles are configured (%v); pick one with --profile",
		slices.Sorted(maps.Keys(cfg.GitHub.Auth)))
}
