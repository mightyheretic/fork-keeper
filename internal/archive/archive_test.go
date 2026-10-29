package archive

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mightyheretic/fork-keeper/internal/config"
	"github.com/mightyheretic/fork-keeper/internal/credentials"
	"github.com/mightyheretic/fork-keeper/internal/git"
	"github.com/mightyheretic/fork-keeper/internal/provider"
	"github.com/mightyheretic/fork-keeper/internal/store"
	"github.com/mightyheretic/fork-keeper/internal/swh"
)

func TestWants(t *testing.T) {
	cases := []struct {
		tag               string
		major, minor, all bool
	}{
		{"v2.0.0", true, true, true},
		{"v2.1.0", false, true, true},
		{"v2.1.3", false, false, true},
		{"2.0", true, true, true},
		{"7.4.0", false, true, true},
		{"v0.5.0", true, true, true}, // before 1.0, minor bumps are the major ones
		{"v0.5.1", false, false, true},
		{"rea-agents-6.0.0", true, true, true},
		{"v3.0.0-rc1", false, false, false},
		{"release-2024-01", false, false, true},
	}
	for _, c := range cases {
		got := [3]bool{Wants(config.ReleasesMajor, c.tag), Wants(config.ReleasesMinor, c.tag), Wants(config.ReleasesAll, c.tag)}
		if want := [3]bool{c.major, c.minor, c.all}; got != want {
			t.Errorf("%s: major/minor/all = %v, want %v", c.tag, got, want)
		}
		if Wants(config.ReleasesNone, c.tag) {
			t.Errorf("%s: none wants it", c.tag)
		}
	}
}

// localHost serves repos from local directories.
type localHost map[string]string

func (h localHost) CloneURL(repo provider.Repo) string { return h[repo.FullName()] }
func (h localHost) GitAuth(context.Context, provider.Repo) (credentials.Secret, error) {
	return credentials.Secret{}, nil
}

type fakeSaver struct{ origins []string }

func (f *fakeSaver) Save(_ context.Context, origin string) (swh.SaveRequest, error) {
	f.origins = append(f.origins, origin)
	return swh.SaveRequest{ID: int64(len(f.origins)), Status: "accepted", TaskStatus: "not yet scheduled"}, nil
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.test",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", file)
	sh(t, dir, "commit", "-q", "-m", "change "+file)
	return sh(t, dir, "rev-parse", "HEAD")
}

type env struct {
	a        *Archiver
	st       *store.Store
	saver    *fakeSaver
	up, fork string // upstream work tree, fork bare repo
	mirror   string
	req      Request
}

func setup(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	e := &env{up: filepath.Join(root, "up"), fork: filepath.Join(root, "fork.git"), mirror: filepath.Join(root, "mirror.git")}
	if err := os.Mkdir(e.up, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, e.up, "init", "-q", "-b", "main")
	sh(t, root, "init", "-q", "--bare", e.fork)
	sh(t, root, "init", "-q", "--bare", e.mirror)

	st, err := store.Open(context.Background(), filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.saver = &fakeSaver{}
	home := filepath.Join(root, "home")
	os.Mkdir(home, 0o700)
	runner := &git.Runner{Home: home, AllowFileProtocol: true}
	hosts := map[string]provider.GitRemote{"github": localHost{"up/app": e.up, "me/app": e.fork}}
	e.a = New(hosts, st, runner, filepath.Join(root, "archive"), nil, e.saver, slog.New(slog.DiscardHandler))
	e.req = Request{
		Provider: "github",
		Upstream: provider.Repo{Owner: "up", Name: "app"},
		Fork:     "me/app",
		Settings: config.ArchiveSettings{
			Releases: config.ReleasesMinor, PushToFork: true, SoftwareHeritage: true,
			Mirrors: []config.Mirror{{URL: e.mirror, Refs: config.MirrorRefsTags}},
		},
	}
	return e
}

func refs(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range strings.Split(sh(t, dir, "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
		if name, sha, ok := strings.Cut(l, " "); ok {
			out[name] = sha
		}
	}
	return out
}

func TestBackupKeepsOriginalTagsAndPushes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c1 := commit(t, e.up, "a.txt", "1")
	sh(t, e.up, "tag", "v1.0.0")
	c2 := commit(t, e.up, "a.txt", "2")
	sh(t, e.up, "tag", "-a", "-m", "release", "v1.1.0")

	req := e.req
	req.Tags = []string{"v1.0.0", "v1.1.0"}
	res, err := e.a.Backup(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"v1.0.0", "v1.1.0"}) || len(res.Existing) != 0 {
		t.Fatalf("first backup: %+v", res)
	}
	for _, p := range res.Pushes {
		if p.Err != nil || p.Refs != 2 {
			t.Fatalf("push %s: %d refs, %v", p.Destination, p.Refs, p.Err)
		}
	}
	fork := refs(t, e.fork)
	if fork["refs/forkkeeper/archive/tags/v1.0.0"] != c1 || fork["refs/tags/v1.0.0"] != "" {
		t.Fatalf("fork should hold hidden refs only: %v", fork)
	}
	if got := sh(t, e.mirror, "rev-parse", "v1.1.0^{commit}"); got != c2 {
		t.Fatalf("mirror v1.1.0 = %s, want %s", got, c2)
	}
	if len(e.saver.origins) != 1 || e.saver.origins[0] != e.up {
		t.Fatalf("Software Heritage requests: %v", e.saver.origins)
	}

	// Upstream rewrites v1.0.0 and deletes v1.1.0: the backups keep the
	// originals, and a tag that never existed is reported missing.
	commit(t, e.up, "a.txt", "rewritten")
	sh(t, e.up, "tag", "-f", "v1.0.0")
	sh(t, e.up, "tag", "-d", "v1.1.0")
	req.Tags = []string{"v1.0.0", "v1.1.0", "v1.2.0"}
	res, err = e.a.Backup(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Existing, []string{"v1.0.0", "v1.1.0"}) || !slices.Equal(res.Missing, []string{"v1.2.0"}) || len(res.Added) != 0 {
		t.Fatalf("second backup: %+v", res)
	}
	local := refs(t, e.a.Dir("github", req.Upstream))
	if local["refs/tags/v1.0.0"] != c1 {
		t.Fatalf("backup of v1.0.0 moved to %s", local["refs/tags/v1.0.0"])
	}
	// Software Heritage was asked recently: not again.
	if len(e.saver.origins) != 1 || !strings.HasPrefix(res.SWHNote, "requested recently") {
		t.Fatalf("Software Heritage: %v, note %q", e.saver.origins, res.SWHNote)
	}
	archived, err := e.st.ArchivedRefs(ctx, "github", "up/app")
	if err != nil || len(archived) != 2 || archived[0].Name != "v1.0.0" || archived[0].CommitSHA != c1 {
		t.Fatalf("archived refs: %+v %v", archived, err)
	}
}

func TestLastOpenAndLastGood(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	commit(t, e.up, "LICENSE", "MIT")
	sh(t, e.up, "tag", "v1.0.0")
	commit(t, e.up, "main.go", "fix 1")
	open := commit(t, e.up, "main.go", "fix 2")
	commit(t, e.up, "LICENSE", "BUSL")
	head := commit(t, e.up, "main.go", "after")

	got, err := e.a.FindLastOpen(ctx, "github", e.req.Upstream, head, []string{"", "v1.0.0"}, []string{"LICENSE"})
	if err != nil {
		t.Fatal(err)
	}
	if got != open {
		t.Fatalf("last open = %s, want %s", got, open)
	}
	// A base after the change finds nothing: fall back to that base.
	if got, err := e.a.FindLastOpen(ctx, "github", e.req.Upstream, head, []string{head}, []string{"LICENSE"}); err != nil || got != head {
		t.Fatalf("fallback = %s, %v", got, err)
	}

	req := e.req
	req.LastGood, req.LastOpen, req.Urgent = "v1.0.0", open, true
	res, err := e.a.Backup(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Added, LastOpenName) || !slices.Contains(res.Added, LastGoodName) {
		t.Fatalf("added: %v", res.Added)
	}
	fork := refs(t, e.fork)
	if fork["refs/forkkeeper/archive/forkkeeper/last-open"] != open {
		t.Fatalf("fork last-open: %v", fork)
	}
	if got := sh(t, e.mirror, "rev-parse", "forkkeeper/last-good"); got != sh(t, e.up, "rev-parse", "v1.0.0") {
		t.Fatalf("mirror last-good = %s", got)
	}

	// Urgent requests always reach Software Heritage.
	if _, err := e.a.Backup(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(e.saver.origins) != 2 {
		t.Fatalf("Software Heritage requests: %v", e.saver.origins)
	}
}

func TestSoftwareHeritageInterval(t *testing.T) {
	e := setup(t)
	commit(t, e.up, "a.txt", "1")
	sh(t, e.up, "tag", "v1.0.0")
	req := e.req
	req.Tags = []string{"v1.0.0"}
	now := time.Now()
	e.a.now = func() time.Time { return now }
	for range 2 {
		if _, err := e.a.Backup(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.saver.origins) != 1 {
		t.Fatalf("requests within the interval: %d", len(e.saver.origins))
	}
	now = now.Add(25 * time.Hour)
	if _, err := e.a.Backup(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(e.saver.origins) != 2 {
		t.Fatalf("requests after the interval: %d", len(e.saver.origins))
	}

	req.Settings.SoftwareHeritage = false
	now = now.Add(48 * time.Hour)
	res, _ := e.a.Backup(context.Background(), req)
	if len(e.saver.origins) != 2 || res.SWHNote != "disabled" {
		t.Fatalf("disabled: %v %q", e.saver.origins, res.SWHNote)
	}
}

func TestPushFailureIsRecordedNotFatal(t *testing.T) {
	e := setup(t)
	commit(t, e.up, "a.txt", "1")
	sh(t, e.up, "tag", "v1.0.0")
	req := e.req
	req.Tags = []string{"v1.0.0"}
	req.Settings.Mirrors = []config.Mirror{{URL: filepath.Join(t.TempDir(), "missing.git"), Refs: config.MirrorRefsHidden}}
	res, err := e.a.Backup(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var failed int
	for _, p := range res.Pushes {
		if p.Err != nil {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("pushes: %+v", res.Pushes)
	}
	pushes, _ := e.st.ArchivePushes(context.Background(), "up/app")
	if len(pushes) != 2 {
		t.Fatalf("recorded pushes: %+v", pushes)
	}
}
