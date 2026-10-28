package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// NamePlaceholder in a Command argument is replaced by the credential name.
const NamePlaceholder = "{name}"

const maxStderr = 512

// Command runs a program and reads the credential from its stdout, for
// example a password manager CLI:
//
//	Argv: []string{"pass", "show", "forkkeeper/{name}"}
//
// The program runs directly, without a shell. Results are cached until
// Reload, so the command runs at most once per credential between reloads.
type Command struct {
	Argv    []string
	Timeout time.Duration // defaults to 10s

	mu    sync.Mutex
	cache map[string]Secret
}

var _ interface {
	Provider
	Reloader
} = (*Command)(nil)

func (*Command) Kind() string { return "command" }

func (p *Command) Reload() {
	p.mu.Lock()
	p.cache = nil
	p.mu.Unlock()
}

func (p *Command) Get(ctx context.Context, name string) (Secret, error) {
	if err := ValidateName(name); err != nil {
		return Secret{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.cache[name]; ok {
		return s, nil
	}
	s, err := p.run(ctx, name)
	if err != nil {
		return Secret{}, err
	}
	if p.cache == nil {
		p.cache = make(map[string]Secret)
	}
	p.cache[name] = s
	return s, nil
}

func (p *Command) run(ctx context.Context, name string) (Secret, error) {
	if len(p.Argv) == 0 {
		return Secret{}, errors.New("command credential provider: no command configured")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := make([]string, len(p.Argv))
	for i, a := range p.Argv {
		argv[i] = strings.ReplaceAll(a, NamePlaceholder, name)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Only stderr goes in the error; stdout may hold a partial secret.
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > maxStderr {
			msg = msg[:maxStderr] + "..."
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		if msg != "" {
			return Secret{}, fmt.Errorf("credential %q: command %s failed: %v: %s", name, argv[0], err, msg)
		}
		return Secret{}, fmt.Errorf("credential %q: command %s failed: %v", name, argv[0], err)
	}
	b := trimNewline(stdout.Bytes())
	if len(b) == 0 {
		return Secret{}, fmt.Errorf("credential %q: %w (command %s printed nothing)", name, ErrNotFound, argv[0])
	}
	return NewSecret(bytes.Clone(b)), nil
}
