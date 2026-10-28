package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExampleConfigLoads(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	example := filepath.Join(filepath.Dir(file), "..", "..", "config", "forkkeeper.example.yaml")
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/secrets")
	t.Setenv("GITHUB_APP_ID", "12345")

	cfg, err := Load(example)
	if err != nil {
		t.Fatal(err)
	}
	app := cfg.GitHub.Auth["default"]
	if app.Type != AuthApp || app.AppID != 12345 || app.PrivateKey != "github_app_private_key" {
		t.Fatalf("default profile = %+v", app)
	}
	if cfg.Credentials.File.Dir != "/run/secrets" {
		t.Fatalf("file dir = %q", cfg.Credentials.File.Dir)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte("credentials:\n  file:\n    dir: /run/secrets\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/var/lib/forkkeeper" || cfg.Database.Path != "/var/lib/forkkeeper/forkkeeper.db" ||
		cfg.Credentials.Provider != "file" || cfg.GitHub.APIURL != "https://api.github.com" ||
		cfg.GitHub.WebhookSecret != "github_webhook_secret" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestExpansion(t *testing.T) {
	t.Setenv("FK_TEST_ID", "777")
	t.Setenv("FK_TEST_DIR", "/creds")
	os.Unsetenv("FK_TEST_UNSET")

	cfg, err := Parse([]byte(`
# Comments with ${FK_TEST_UNSET} are not expanded.
data_dir: ${FK_TEST_UNSET:-/srv/fk}
credentials:
  file:
    dir: ${FK_TEST_DIR}
github:
  auth:
    default:
      type: app
      app_id: ${FK_TEST_ID}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/srv/fk" || cfg.Credentials.File.Dir != "/creds" || cfg.GitHub.Auth["default"].AppID != 777 {
		t.Fatalf("expansion failed: %+v", cfg)
	}

	_, err = Parse([]byte("credentials:\n  file:\n    dir: ${FK_TEST_UNSET}\n"))
	if err == nil || !strings.Contains(err.Error(), "FK_TEST_UNSET is not set") {
		t.Fatalf("unset var: err = %v", err)
	}
}

func TestStrictAndValidation(t *testing.T) {
	tests := map[string]string{
		"unknown field":      "credentals:\n  provider: file\n",
		"bad provider":       "credentials:\n  provider: vault\n",
		"file without dir":   "credentials:\n  provider: file\n",
		"command no argv":    "credentials:\n  provider: command\n",
		"app without id":     "credentials: {provider: env}\ngithub:\n  auth:\n    default: {type: app}\n",
		"pat without token":  "credentials: {provider: env}\ngithub:\n  auth:\n    default: {type: pat}\n",
		"bad auth type":      "credentials: {provider: env}\ngithub:\n  auth:\n    default: {type: oauth}\n",
		"bad pattern":        "credentials: {provider: env}\ngithub:\n  auth:\n    default: {type: pat, token: t}\n  repo_auth:\n    \"a/b/c\": default\n",
		"unknown profile":    "credentials: {provider: env}\ngithub:\n  repo_auth:\n    \"a/*\": nope\n",
		"traversal key name": "credentials: {provider: env}\ngithub:\n  auth:\n    default: {type: pat, token: ../x}\n",
		"bad api url":        "credentials: {provider: env}\ngithub:\n  api_url: not-a-url\n",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestCommandTimeout(t *testing.T) {
	cfg, err := Parse([]byte("credentials:\n  provider: command\n  command:\n    argv: [pass, show, '{name}']\n    timeout: 3s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Credentials.Command.Timeout != 3*time.Second || len(cfg.Credentials.Command.Argv) != 3 {
		t.Fatalf("command = %+v", cfg.Credentials.Command)
	}
}

func TestAuthProfileFor(t *testing.T) {
	g := GitHub{
		Auth: map[string]GitHubAuth{"default": {}, "work": {}, "special": {}},
		RepoAuth: map[string]string{
			"acme/*":       "work",
			"acme/special": "special",
		},
	}
	for repo, want := range map[string]string{
		"acme/special": "special",
		"acme/other":   "work",
		"me/thing":     "default",
	} {
		if got := g.AuthProfileFor(repo); got != want {
			t.Errorf("AuthProfileFor(%q) = %q, want %q", repo, got, want)
		}
	}

	g.RepoAuth["*"] = "work"
	if got := g.AuthProfileFor("me/thing"); got != "work" {
		t.Errorf("wildcard should beat the default profile, got %q", got)
	}

	if got := (GitHub{}).AuthProfileFor("me/thing"); got != "" {
		t.Errorf("no profiles: got %q", got)
	}
}

func TestEmptyFile(t *testing.T) {
	cfg, err := Parse([]byte("credentials: {provider: env}\n"))
	if err != nil || cfg.Credentials.Provider != "env" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	// A truly empty file fails validation only because file.dir is required.
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "credentials.file.dir") {
		t.Fatalf("empty file: err = %v", err)
	}
}
