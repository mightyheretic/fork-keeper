package credentials

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// DefaultEnvPrefix is prepended to credential names to form variable names.
const DefaultEnvPrefix = "FORKKEEPER_CRED_"

// Env reads credentials from environment variables. The name
// "github_webhook_secret" maps to FORKKEEPER_CRED_GITHUB_WEBHOOK_SECRET.
type Env struct {
	// Prefix defaults to DefaultEnvPrefix.
	Prefix string
}

var _ Provider = Env{}

func (Env) Kind() string { return "env" }

// VarName returns the environment variable that holds name.
func (p Env) VarName(name string) string {
	prefix := p.Prefix
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}
	return prefix + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

func (p Env) Get(_ context.Context, name string) (Secret, error) {
	if err := ValidateName(name); err != nil {
		return Secret{}, err
	}
	v, ok := os.LookupEnv(p.VarName(name))
	if !ok || v == "" {
		return Secret{}, fmt.Errorf("credential %q: %w (set %s)", name, ErrNotFound, p.VarName(name))
	}
	return NewSecret(trimNewline([]byte(v))), nil
}
