package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/license"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/provider/github"
)

const licenseUsage = `usage:
  forkkeeper license scan <dir | owner/repo@ref>
  forkkeeper license diff <owner/repo> <old-ref> <new-ref>
  forkkeeper license diff <dir | owner/repo@ref> <dir | owner/repo@ref>

diff exits 3 when the top-level license changed, 4 when only sub-licenses
(nested license files or file headers) changed, and 0 otherwise.`

// licenseCmd scans or diffs licenses without touching the database.
// Remote refs are downloaded as tarballs using the poll auth profile.
func licenseCmd(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, licenseUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	sc, err := newCLIScanner()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	switch args[0] {
	case "scan":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, licenseUsage)
			return 2
		}
		r, err := sc.scan(ctx, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		r.WriteText(os.Stdout)
		return 0

	case "diff":
		var oldT, newT string
		switch len(args) {
		case 3:
			oldT, newT = args[1], args[2]
		case 4:
			oldT, newT = args[1]+"@"+args[2], args[1]+"@"+args[3]
		default:
			fmt.Fprintln(os.Stderr, licenseUsage)
			return 2
		}
		oldR, err := sc.scan(ctx, oldT)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		newR, err := sc.scan(ctx, newT)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		d := license.Compare(refLabel(oldT), oldR, refLabel(newT), newR)
		d.WriteText(os.Stdout)
		// Distinct exit codes so scripts can detect a change.
		switch {
		case d.TopLevelChanged:
			return 3
		case d.Changed:
			return 4 // only nested license files or file headers changed
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, licenseUsage)
	return 2
}

type cliScanner struct {
	archives provider.ArchiveFetcher
}

// newCLIScanner uses the config's poll auth when a config is available,
// and falls back to anonymous downloads otherwise.
func newCLIScanner() (*cliScanner, error) {
	var token github.TokenFunc
	apiURL := "https://api.github.com"
	if _, err := os.Stat(getenv("FORKKEEPER_CONFIG", "/etc/forkkeeper/config.yaml")); err == nil {
		cfg, err := loadConfig()
		if err != nil {
			return nil, err
		}
		apiURL = cfg.GitHub.APIURL
		token = upstreamToken(cfg, newGitHubAuth(cfg, newCredentials(cfg.Credentials)))
	}
	return &cliScanner{archives: github.NewArchives(apiURL, token)}, nil
}

func (c *cliScanner) scan(ctx context.Context, target string) (*license.Report, error) {
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return license.Scan(ctx, license.Dir(target), license.Options{})
	}
	repoName, ref, ok := strings.Cut(target, "@")
	repo, err := provider.ParseRepo(repoName)
	if !ok || ref == "" || err != nil {
		return nil, fmt.Errorf("%q is neither a directory nor owner/repo@ref", target)
	}
	fmt.Fprintf(os.Stderr, "downloading %s@%s...\n", repo.FullName(), ref)
	body, err := c.archives.Archive(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return license.Scan(ctx, license.TarGz{R: body, StripComponents: 1}, license.Options{})
}

func refLabel(target string) string {
	if _, ref, ok := strings.Cut(target, "@"); ok {
		return ref
	}
	return target
}
