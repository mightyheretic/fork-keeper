package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestWatcherAndRepos(t *testing.T) {
	cfg, err := Parse([]byte(`
credentials: {provider: env}
github:
  auth:
    default: {type: app, app_id: 1}
    personal: {type: pat, token: github_pat}
  repo_auth:
    "acme/*": personal
watcher:
  include_prereleases: true
repos:
  - upstream: up/app
  - upstream: acme/lib
    watch: tags
    include_prereleases: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Watcher.PollInterval != 15*time.Minute || cfg.Repos[0].Watch != WatchReleases || cfg.Repos[1].Watch != WatchTags {
		t.Fatalf("defaults not applied: %+v", cfg.Watcher)
	}
	if !cfg.WantsPrereleases(cfg.Repos[0]) || cfg.WantsPrereleases(cfg.Repos[1]) {
		t.Fatal("include_prereleases override not applied")
	}
	if cfg.PollAuthProfile("up/app") != "default" || cfg.PollAuthProfile("acme/lib") != "personal" {
		t.Fatal("poll auth should follow repo_auth when watcher.auth is unset")
	}
	cfg.Watcher.Auth = Anonymous
	if cfg.PollAuthProfile("acme/lib") != "" {
		t.Fatal("anonymous should override repo_auth")
	}
	cfg.Watcher.Auth = "personal"
	if cfg.PollAuthProfile("up/app") != "personal" {
		t.Fatal("watcher.auth should apply to every upstream")
	}

	for name, raw := range map[string]string{
		"bad upstream":  "repos:\n  - upstream: just-a-name\n",
		"duplicate":     "repos:\n  - upstream: up/app\n  - upstream: UP/app\n",
		"bad watch":     "repos:\n  - upstream: up/app\n    watch: commits\n",
		"fast interval": "watcher:\n  poll_interval: 10s\n",
		"unknown auth":  "watcher:\n  auth: nope\n",
		"bad fork":      "repos:\n  - upstream: up/app\n    fork: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte("credentials: {provider: env}\n" + raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestMergeConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
credentials: {provider: env}
repos:
  - upstream: up/a
    fork: me/a
  - upstream: up/b
    fork: me/b
    merge_strategy: rebase
    stable_branch: release
    staging_branch: sync/upstream
merge:
  auto_merge: false
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Repos[0], cfg.Repos[1]
	if a.MergeStrategy != StrategyMerge || a.StagingBranch != DefaultStagingBranch || a.StableBranch != "" {
		t.Fatalf("defaults = %+v", a)
	}
	if b.MergeStrategy != StrategyPatchStack || b.StagingBranch != "sync/upstream" {
		t.Fatalf("rebase alias = %+v", b)
	}
	if cfg.Merge.AutoMergeEnabled() || !cfg.Merge.CIRequired() || cfg.Merge.CheckInterval != 5*time.Minute {
		t.Fatalf("merge = %+v", cfg.Merge)
	}
	if !cfg.IsFork("ME/b") || cfg.IsFork("up/a") {
		t.Fatal("IsFork")
	}

	for name, repo := range map[string]string{
		"strategy":     "merge_strategy: octopus",
		"branch":       "staging_branch: 'bad..name'",
		"same":         "stable_branch: main\n    staging_branch: main",
		"lock suffix":  "staging_branch: x.lock",
		"leading dash": "staging_branch: -x",
	} {
		_, err := Parse([]byte("credentials: {provider: env}\nrepos:\n  - upstream: up/a\n    fork: me/a\n    " + repo + "\n"))
		if err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	dup := "credentials: {provider: env}\nrepos:\n  - {upstream: up/a, fork: me/x}\n  - {upstream: up/b, fork: me/x}\n"
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("duplicate fork: %v", err)
	}
}

func TestArchiveConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
credentials: {provider: env}
watcher:
  main_branch: false
archive:
  software_heritage_token: swh_token
  mirrors:
    - url: https://codeberg.org/me/{name}.git
      token: codeberg_token
      username: me
repos:
  - upstream: up/a
    fork: me/a
    main_branch: true
  - upstream: up/b
    archive:
      releases: major
      push_to_fork: false
      software_heritage: false
      mirrors:
        - url: https://gitlab.example.com/backup/{owner}-{name}.git
          refs: tags
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Repos[0], cfg.Repos[1]
	if !cfg.WatchesMainBranch(a) || cfg.WatchesMainBranch(b) {
		t.Fatal("main_branch override")
	}
	sa := cfg.ArchiveFor(a)
	if sa.Releases != ReleasesMinor || !sa.PushToFork || !sa.SoftwareHeritage || len(sa.Mirrors) != 1 ||
		sa.Mirrors[0].URL != "https://codeberg.org/me/a.git" || sa.Mirrors[0].Username != "me" || sa.Mirrors[0].Refs != MirrorRefsHidden {
		t.Fatalf("repo a: %+v", sa)
	}
	sb := cfg.ArchiveFor(b)
	if sb.Releases != ReleasesMajor || sb.PushToFork || sb.SoftwareHeritage || len(sb.Mirrors) != 2 ||
		sb.Mirrors[1].URL != "https://gitlab.example.com/backup/up-b.git" || sb.Mirrors[1].Username != "x-access-token" {
		t.Fatalf("repo b: %+v", sb)
	}
	names := cfg.CredentialNames()
	if !slices.Contains(names, "swh_token") || !slices.Contains(names, "codeberg_token") {
		t.Fatalf("credential names: %v", names)
	}
	if def, _ := Parse([]byte("credentials: {provider: env}\nrepos: [{upstream: up/a}]\n")); !def.WatchesMainBranch(def.Repos[0]) {
		t.Fatal("main branch watch should default on")
	}

	for name, bad := range map[string]string{
		"policy":     "archive: {releases: patch}",
		"http":       "archive: {mirrors: [{url: 'http://example.com/x.git'}]}",
		"userinfo":   "archive: {mirrors: [{url: 'https://me:secret@example.com/x.git'}]}",
		"layout":     "archive: {mirrors: [{url: 'https://example.com/x.git', refs: branches}]}",
		"repo field": "repos: [{upstream: up/a, archive: {software_heritage_token: x}}]",
	} {
		if _, err := Parse([]byte("credentials: {provider: env}\n" + bad + "\n")); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNotificationsConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
credentials: {provider: env}
notifications:
  ntfy:
    url: https://ntfy.sh/fk-alerts
    token: ntfy_token
  gotify:
    url: http://100.101.102.103:8080
    token: gotify_token
  email:
    smtp: smtp.example.com:587
    username: me
    password: smtp_password
    from: Fork Keeper <fk@example.com>
    to: [me@example.com]
  remind_after: 48h
  sync_problems: false
`))
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Notifications
	if !n.Enabled() || n.Reminders() != 48*time.Hour || n.NotifySyncProblems() || !n.NotifyApprovals() {
		t.Fatalf("notifications = %+v", n)
	}
	names := cfg.CredentialNames()
	for _, want := range []string{"ntfy_token", "gotify_token", "smtp_password"} {
		if !slices.Contains(names, want) {
			t.Errorf("credential names %v missing %s", names, want)
		}
	}
	if (Notifications{}).Reminders() != DefaultRemindAfter {
		t.Error("default reminder interval")
	}

	for _, bad := range []string{
		"ntfy: {url: http://ntfy.example.com/topic}",                // http over the internet
		"ntfy: {url: https://ntfy.sh/}",                             // no topic
		"ntfy: {url: 'https://me:pw@ntfy.sh/topic'}",                // credentials in the URL
		"gotify: {url: https://gotify.example.com}",                 // no token
		"email: {smtp: smtp.example.com, from: a@b.c, to: [d@e.f]}", // no port
		"email: {smtp: 'smtp.example.com:587', from: a@b.c, to: []}",
		"webhook: {url: https://example.com/hook, token: '../x'}",
		"remind_after: 5m",
	} {
		if _, err := Parse([]byte("credentials: {provider: env}\nnotifications:\n  " + bad + "\n")); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Parse([]byte("credentials: {provider: env}\nnotifications:\n  webhook: {url: 'http://127.0.0.1:9000/hook'}\n")); err != nil {
		t.Errorf("loopback http rejected: %v", err)
	}
}
