package credentials

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File reads each credential from a file named after it in Dir, for example
// $CREDENTIALS_DIRECTORY from systemd or /run/secrets from Compose.
//
// Files are re-read on every Get, so a replaced file takes effect immediately.
type File struct {
	Dir string
	// Prefix is prepended to each name to get the file name. The systemd
	// unit imports forkkeeper.* from the encrypted credential store, so
	// github_pat is read from $CREDENTIALS_DIRECTORY/forkkeeper.github_pat.
	Prefix string
}

var _ interface {
	Provider
	Checker
} = File{}

func (File) Kind() string { return "file" }

func (p File) path(name string) (string, error) {
	if p.Dir == "" {
		return "", errors.New("file credential provider: no directory configured")
	}
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if err := ValidateName(p.Prefix + name); err != nil {
		return "", fmt.Errorf("file credential provider: bad prefix %q: %w", p.Prefix, err)
	}
	return filepath.Join(p.Dir, p.Prefix+name), nil
}

func (p File) Get(_ context.Context, name string) (Secret, error) {
	path, err := p.path(name)
	if err != nil {
		return Secret{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Secret{}, fmt.Errorf("credential %q: %w (looked for %s)", name, ErrNotFound, path)
	}
	if err != nil {
		return Secret{}, fmt.Errorf("credential %q: %w", name, err)
	}
	return NewSecret(trimNewline(b)), nil
}

// Check warns when a credential file is group- or world-accessible.
func (p File) Check(name string) ([]string, error) {
	path, err := p.path(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("credential %q: %w", name, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return []string{fmt.Sprintf("credential file %s has mode %04o; it should be readable by its owner only (chmod 600)", path, perm)}, nil
	}
	return nil, nil
}
