// Package credentials loads secrets from a pluggable backend (file, env or
// command). Callers ask for a credential by name, for example
// "github_app_private_key", and never learn where it came from.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
)

// Secret holds credential bytes. It redacts itself when printed, logged or
// marshalled, so a stray log line can't leak it. Use Bytes or Reveal to get
// at the value.
type Secret struct{ b []byte }

// NewSecret wraps b.
func NewSecret(b []byte) Secret { return Secret{b: b} }

// Bytes returns the secret value.
func (s Secret) Bytes() []byte { return s.b }

// Reveal returns the secret value as a string.
func (s Secret) Reveal() string { return string(s.b) }

// Empty reports whether the secret has no content.
func (s Secret) Empty() bool { return len(s.b) == 0 }

const redacted = "[REDACTED]"

func (Secret) String() string                { return redacted }
func (Secret) GoString() string              { return redacted }
func (Secret) LogValue() slog.Value          { return slog.StringValue(redacted) }
func (Secret) MarshalJSON() ([]byte, error)  { return []byte(`"` + redacted + `"`), nil }
func (Secret) MarshalText() ([]byte, error)  { return []byte(redacted), nil }
func (Secret) Format(f fmt.State, verb rune) { f.Write([]byte(redacted)) }

// ErrNotFound is returned when the backend has no credential with that name.
var ErrNotFound = errors.New("credential not found")

// Provider is a credential backend.
type Provider interface {
	// Get returns the named credential. Implementations wrap ErrNotFound
	// when it does not exist, and must never include the value in errors.
	Get(ctx context.Context, name string) (Secret, error)
	// Kind is the backend name ("file", "env", "command") for logs.
	Kind() string
}

// Reloader is implemented by providers that cache values.
type Reloader interface {
	// Reload drops cached values so the next Get fetches fresh ones.
	Reload()
}

// Checker is implemented by providers that can audit how a credential is
// stored, for example file permissions.
type Checker interface {
	// Check returns human-readable warnings about the named credential.
	Check(name string) ([]string, error)
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidateName rejects names that could escape a directory or be
// misinterpreted by a backend, such as "../x" or "a/b".
func ValidateName(name string) error {
	if !validName.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid credential name %q: use letters, digits, '_', '.' and '-'", name)
	}
	return nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
