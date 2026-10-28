package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const value = "hunter2-very-secret"

func TestSecretRedacts(t *testing.T) {
	s := NewSecret([]byte(value))
	type wrapper struct{ Token Secret }

	outputs := map[string]string{
		"%v":  fmt.Sprintf("%v", s),
		"%s":  fmt.Sprintf("%s", s),
		"%q":  fmt.Sprintf("%q", s),
		"%x":  fmt.Sprintf("%x", s),
		"%+v": fmt.Sprintf("%+v", wrapper{s}),
		"%#v": fmt.Sprintf("%#v", wrapper{s}),
	}
	b, _ := json.Marshal(wrapper{s})
	outputs["json"] = string(b)
	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("x", "secret", s, "err", fmt.Errorf("wrapped %v", s))
	outputs["slog"] = logBuf.String()

	for name, out := range outputs {
		if strings.Contains(out, value) || strings.Contains(out, fmt.Sprintf("%x", value)) {
			t.Errorf("%s leaked the secret: %s", name, out)
		}
	}
	if s.Reveal() != value || string(s.Bytes()) != value {
		t.Fatal("Reveal/Bytes do not return the value")
	}
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "/abs", ".hidden", "a b", "a\x00b"} {
		if ValidateName(bad) == nil {
			t.Errorf("ValidateName(%q) accepted a bad name", bad)
		}
	}
	for _, good := range []string{"github_app_private_key", "key.pem", "a-b", "X1"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("ValidateName(%q) = %v", good, err)
		}
	}
}

func TestFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tok"), []byte(value+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "loose"), []byte(value), 0o644)
	p := File{Dir: dir}
	ctx := context.Background()

	s, err := p.Get(ctx, "tok")
	if err != nil || s.Reveal() != value {
		t.Fatalf("Get = %q, %v (trailing newline should be trimmed)", s.Reveal(), err)
	}
	if _, err := p.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
	if _, err := p.Get(ctx, "../tok"); err == nil {
		t.Fatal("path traversal accepted")
	}

	// A replaced file is picked up without any reload.
	os.WriteFile(filepath.Join(dir, "tok"), []byte("rotated"), 0o600)
	if s, _ := p.Get(ctx, "tok"); s.Reveal() != "rotated" {
		t.Fatalf("rotated value not picked up: %q", s.Reveal())
	}

	if w, err := p.Check("tok"); err != nil || len(w) != 0 {
		t.Fatalf("Check(0600) = %v, %v", w, err)
	}
	if w, err := p.Check("loose"); err != nil || len(w) != 1 {
		t.Fatalf("Check(0644) = %v, %v; want one warning", w, err)
	}
}

func TestEnv(t *testing.T) {
	p := Env{}
	if got := p.VarName("github_app-key.pem"); got != "FORKKEEPER_CRED_GITHUB_APP_KEY_PEM" {
		t.Fatalf("VarName = %q", got)
	}
	t.Setenv("FORKKEEPER_CRED_TOK", value)
	if s, err := p.Get(context.Background(), "tok"); err != nil || s.Reveal() != value {
		t.Fatalf("Get = %q, %v", s.Reveal(), err)
	}
	if _, err := p.Get(context.Background(), "unset"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset: err = %v", err)
	}
	if _, err := (Env{Prefix: "MY_"}).Get(context.Background(), "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("custom prefix should not read FORKKEEPER_CRED_TOK: %v", err)
	}
}

func TestCommandCachesUntilReload(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	p := &Command{Argv: []string{"sh", "-c", `echo run >> "$1"; printf 'secret-for-%s\n' "$2"`, "sh", counter, "{name}"}}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		s, err := p.Get(ctx, "tok")
		if err != nil || s.Reveal() != "secret-for-tok" {
			t.Fatalf("Get = %q, %v", s.Reveal(), err)
		}
	}
	runs := func() int {
		b, _ := os.ReadFile(counter)
		return strings.Count(string(b), "run")
	}
	if n := runs(); n != 1 {
		t.Fatalf("command ran %d times, want 1 (cached)", n)
	}
	p.Reload()
	p.Get(ctx, "tok")
	if n := runs(); n != 2 {
		t.Fatalf("command ran %d times after Reload, want 2", n)
	}
}

func TestCommandFailures(t *testing.T) {
	ctx := context.Background()

	p := &Command{Argv: []string{"sh", "-c", `printf 'partial-secret'; echo 'vault sealed' >&2; exit 3`}}
	_, err := p.Get(ctx, "tok")
	if err == nil || !strings.Contains(err.Error(), "vault sealed") {
		t.Fatalf("err = %v, want stderr included", err)
	}
	if strings.Contains(err.Error(), "partial-secret") {
		t.Fatalf("error leaked stdout: %v", err)
	}

	p = &Command{Argv: []string{"true"}}
	if _, err := p.Get(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty output: err = %v", err)
	}

	p = &Command{Argv: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := p.Get(ctx, "tok"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}

	// Failures are not cached.
	p = &Command{Argv: []string{"false"}}
	p.Get(ctx, "tok")
	if p.cache != nil {
		t.Fatal("failed result was cached")
	}
}
