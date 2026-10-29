package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

func runner(t *testing.T) *Runner {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	return &Runner{Home: t.TempDir(), Identity: Identity{Name: "FK", Email: "fk@example.test"}}
}

func TestOnlyHTTPSRemotes(t *testing.T) {
	r := runner(t)
	ctx := context.Background()
	src, dst := t.TempDir(), t.TempDir()
	if _, err := r.Run(ctx, Cmd{Dir: src, Args: []string{"init", "-q"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, Cmd{Dir: dst, Args: []string{"init", "-q", "--bare"}}); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{src, "file://" + src, "ext::sh -c touch% /tmp/pwned", "ssh://example.test/x"} {
		_, err := r.Run(ctx, Cmd{Dir: dst, Args: []string{"fetch", "-q", url}})
		var gerr *Error
		if !errors.As(err, &gerr) || !strings.Contains(gerr.Stderr, "not allowed") {
			t.Errorf("fetch %q: err = %v, want transport not allowed", url, err)
		}
	}
}

func TestHooksNeverRun(t *testing.T) {
	r := runner(t)
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := r.Run(ctx, Cmd{Dir: dir, Args: []string{"init", "-q"}}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	r.Run(ctx, Cmd{Dir: dir, Args: []string{"add", "f"}})
	if _, err := r.Run(ctx, Cmd{Dir: dir, Args: []string{"commit", "-q", "-m", "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook ran")
	}
}

func TestAuthRedactedAndNotInArgv(t *testing.T) {
	// A stand-in git that prints its argv and the auth header to stderr, then fails.
	bin := filepath.Join(t.TempDir(), "git")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"args: $*\" >&2\necho \"header: $GIT_CONFIG_VALUE_0\" >&2\nexit 1\n"), 0o755)
	r := &Runner{Bin: bin, Home: t.TempDir()}
	secret := credentials.NewSecret([]byte("Basic c2VjcmV0LXRva2Vu"))

	_, err := r.Run(context.Background(), Cmd{Dir: t.TempDir(), Auth: secret, Args: []string{"fetch", "https://example.test/x"}})
	var gerr *Error
	if !errors.As(err, &gerr) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "c2VjcmV0LXRva2Vu") {
		t.Fatalf("secret leaked: %v", err)
	}
	if !strings.Contains(gerr.Stderr, "header: Authorization: [redacted]") {
		t.Fatalf("header not passed through the environment: %q", gerr.Stderr)
	}
	if !strings.Contains(gerr.Stderr, "core.hooksPath=/dev/null") || strings.Contains(gerr.Stderr, "args: -c http") {
		t.Fatalf("unexpected argv: %q", gerr.Stderr)
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.lock")
	unlock, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(context.Background(), path); err == nil {
		t.Fatal("second lock succeeded")
	}
	unlock()
	again, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
