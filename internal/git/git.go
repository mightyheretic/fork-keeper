// Package git runs the git binary against untrusted upstream content.
//
// Every command runs with a config that ignores system and user git config,
// disables hooks, fsmonitor and submodule recursion, and allows only HTTPS
// remotes, so nothing from an upstream repository is ever executed. Tokens
// are passed per command through the environment, never in argv or in a
// persisted remote URL.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/mightyheretic/fork-keeper/internal/credentials"
)

// Identity is the committer used for merge and cherry-picked commits.
type Identity struct {
	Name  string
	Email string
}

// Runner runs git commands.
type Runner struct {
	// Bin is the git executable. Default "git".
	Bin string
	// Home is a private, writable directory used as $HOME.
	Home     string
	Identity Identity
	// AllowFileProtocol permits local-path remotes. Tests only.
	AllowFileProtocol bool
}

// Error is a failed git command. Stderr has any credential redacted.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 2000 {
		msg = msg[:2000] + "..."
	}
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.ExitCode, msg)
}

// Cmd describes one git invocation.
type Cmd struct {
	Dir  string
	Args []string
	// Auth is an HTTP Authorization header value sent to the remote, or empty.
	Auth credentials.Secret
	// Env adds variables, such as GIT_AUTHOR_NAME for commit-tree.
	Env []string
	// Stdin is passed to the command, if set.
	Stdin string
}

// Run runs c and returns its stdout.
func (r *Runner) Run(ctx context.Context, c Cmd) (string, error) {
	bin := r.Bin
	if bin == "" {
		bin = "git"
	}
	args := append(r.safetyConfig(), c.Args...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = c.Dir
	cmd.Env = append(r.env(c.Auth), c.Env...)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the whole process group on cancel: git spawns helpers.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", c.Args[0], ctx.Err())
		} else {
			return "", fmt.Errorf("git %s: %w", c.Args[0], err)
		}
		msg := stderr.String()
		if !c.Auth.Empty() {
			msg = strings.ReplaceAll(msg, c.Auth.Reveal(), "[redacted]")
		}
		return stdout.String(), &Error{Args: c.Args, ExitCode: code, Stderr: msg}
	}
	return stdout.String(), nil
}

// Lines runs c and splits stdout into non-empty lines.
func (r *Runner) Lines(ctx context.Context, c Cmd) ([]string, error) {
	out, err := r.Run(ctx, c)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

func splitLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// safetyConfig is passed as -c options so it applies regardless of any
// repository-local config.
func (r *Runner) safetyConfig() []string {
	kv := []string{
		"core.hooksPath=/dev/null",
		"core.fsmonitor=false",
		"core.untrackedCache=false",
		"core.askPass=",
		"credential.helper=",
		"submodule.recurse=false",
		"fetch.recurseSubmodules=false",
		"protocol.allow=never",
		"protocol.https.allow=always",
		"gc.auto=0",
		"advice.detachedHead=false",
		"init.defaultBranch=main",
		"merge.conflictStyle=merge",
		"rerere.enabled=false",
		"user.useConfigOnly=true",
		"user.name=" + r.Identity.Name,
		"user.email=" + r.Identity.Email,
	}
	if r.AllowFileProtocol {
		kv = append(kv, "protocol.file.allow=always")
	}
	args := make([]string, 0, 2*len(kv))
	for _, s := range kv {
		args = append(args, "-c", s)
	}
	return args
}

func (r *Runner) env(auth credentials.Secret) []string {
	env := []string{
		"HOME=" + r.Home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
	}
	if v := os.Getenv("TMPDIR"); v != "" {
		env = append(env, "TMPDIR="+v)
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if !auth.Empty() {
		// Environment config (git 2.31+): not visible in argv, not persisted.
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: "+auth.Reveal(),
		)
	}
	return env
}

// Lock takes an exclusive lock on path (creating it), so the service and a
// CLI run don't use the same working copy at once. Call the returned
// function to release it.
func Lock(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s is in use by another forkkeeper process", filepath.Dir(path))
		}
		return nil, err
	}
	f.Truncate(0)
	f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
