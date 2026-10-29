# Fork Keeper

Self-hosted service that keeps maintained forks of the open-source apps you depend on.
When upstream publishes a release, Fork Keeper checks for license changes first, then merges
upstream into your fork on a staging branch, keeping history, and opens a PR. If a license
change breaks your rules, it applies the action you configured, such as freezing the fork,
archiving it or notifying you.

See [docs/spec.md](docs/spec.md) for the full specification.

**Status:** V1 milestones 1–7 done.
- **Watching:** the service polls upstream repos for new releases (or tags) and receives verified GitHub webhooks. Each new release is queued once in SQLite.
- **License checks:** for each new release, it compares the licenses against the previous stable version.
- **Rules:** your license rules decide what happens. A repo can be frozen at its last good version, and each version gets a merge policy for the merge engine.
- **Merging:** each new release is merged into your fork on a staging branch, keeping history, and a PR is opened. `auto` PRs are merged once CI passes; anything else waits for you.
- **Backups:** the source of each minor release (git history and tags, no release assets) is kept locally, pushed to your fork and mirrors, and Software Heritage is asked to archive the upstream.
- **Early warning:** each upstream's default branch is checked between releases, so a relicense is caught before it ships and the last commit before it is backed up.
- **GitHub access:** it authenticates as a GitHub App or with a fine-grained PAT, using pluggable credential backends.

Notifications land in a later milestone. Until then `notify` is recorded as pending.

## Upstream polling

GitHub only sends webhooks for repos where your App is installed, and that's normally your forks,
not the upstreams you follow. So Fork Keeper finds upstream releases by polling. List them under
`repos:` in the config:

```yaml
repos:
  - upstream: hashicorp/terraform
  - upstream: git/git
    watch: tags          # for projects that tag but don't publish GitHub releases
```

- Each upstream is polled every `watcher.poll_interval` (default 15m), using conditional requests (ETags).
- The first poll of a repo only records the releases that already exist, so adding a repo doesn't replay its history.
- After that, each new release is queued once, oldest first. Pre-releases are queued only if `include_prereleases` is on.
- When the rate limit runs out, polling pauses until it resets.

Polling works without credentials, but GitHub allows anonymous clients only 60 requests an hour,
and every anonymous request counts, even "not modified" ones. For more than a handful of repos,
create a fine-grained PAT with **Public repositories (read-only)** access. Save it as
`secrets/github_pat`, add a `type: pat` profile, and set `watcher.auth` to that profile.

Check access and see the latest releases without changing anything:

```sh
docker compose run --rm forkkeeper poll --dry-run
docker compose run --rm forkkeeper poll --repo owner/name   # one real poll
```

## License detection

For each new upstream release, Fork Keeper downloads the source tarball for that version and for
the previous stable version (by version order). It scans each one as a stream, without
extracting it or running anything from it. Each version's result is cached, so it's only
downloaded once. Each scan looks at:

- **License files** (`LICENSE*`, `COPYING*`, `UNLICENSE`, `LICENSES/`) anywhere in the tree.
  Nested ones count too, e.g. an `ee/LICENSE` for enterprise code. The text is identified with
  [licensecheck](https://github.com/google/licensecheck). Distinctive phrases catch
  source-available licenses it doesn't know: BUSL-1.1, Elastic-2.0, SSPL-1.0, RSALv2, FSL and
  Commons Clause.
- **Root manifests** (`package.json`, `composer.json`, `Cargo.toml`, `pyproject.toml`), with a warning
  when they disagree with the license files.
- **File headers:** `SPDX-License-Identifier` tags and standard license notices in source files.

`vendor/`, `node_modules/`, `third_party/` and `testdata/` are skipped. A copyright-year bump is
not a change. Extra terms added to a known license are.

You can also try it by hand, without touching the database:

```sh
forkkeeper license scan owner/repo@v1.2.3          # or a local directory
forkkeeper license diff hashicorp/terraform v1.5.5 v1.6.0
```

`diff` exits with code 3 when licenses changed. Verified against real relicenses: Terraform
(MPL-2.0 → BUSL-1.1), Redis (BSD-3-Clause → RSALv2/SSPL, then + AGPL-3.0) and Elasticsearch
(Apache-2.0 → Elastic-2.0/SSPL).

## License rules

Rules live under `license:` in the config (see `config/forkkeeper.example.yaml`). Each one maps a
transition to actions:

```yaml
license:
  allowlist: [MIT, Apache-2.0, BSD-3-Clause]
  default_actions: [require_approval, notify]   # licenses changed, but no rule matched
  rules:
    - name: relicense-to-source-available
      from: [MIT, Apache-2.0]          # licenses in the previous version (default: any)
      to: [BUSL-1.1, SSPL-1.0, FSL-*]  # licenses added in the new version
      actions: [freeze, archive, notify]
    - name: gpl-dropped
      removed: [GPL-*]                 # licenses dropped in the new version
      actions: [notify]
    - name: our-db-is-fine
      repos: [redis/redis]             # or groups: [...]; neither means global
      to: [any]
      actions: [allow]
```

- **Patterns:** SPDX IDs (aliases like `BSL-1.1` are accepted), globs, `any`, `not-allowed` (outside the allowlist) and `unknown` (no license, or one that couldn't be identified).
- **What counts as a license change:** rules run when the project's own license changes. That covers the declared expression, root license files and manifests. Nested license files (such as `ee/LICENSE` or vendored third-party licenses) and file headers are sub-licenses, and each repo's `sub_licenses` setting decides how they count:

  | `sub_licenses` | Effect |
  |---|---|
  | `escalate` (default) | Ignored, unless a sub-license brings in a license that's new to the project and not on the allowlist (a BUSL `ee/` folder, say). That runs `escalation_actions`, by default require approval, flag the PR and notify |
  | `ignore` | Recorded, never acted on |
  | `all` | Any sub-license change counts, and rules also match licenses added in nested files and headers |
- **Which rule wins:** the most specific rule (repo > group > global), then the highest `priority`, then the earliest in the file.
- **New repos:** a repo's first version is checked against the allowlist.

| Action | Effect |
|---|---|
| `freeze` | The fork stays at the last version that passed. Takes effect immediately |
| `block_auto_merge` | The PR is opened but never merged automatically |
| `require_approval` | The PR is opened as a draft and merges only after you approve it |
| `flag_pr` | The PR gets the `forkkeeper: license flagged` label |
| `archive` | Backs up the version before the change as `forkkeeper/last-good`, whatever `archive.releases` says, and asks Software Heritage to archive the upstream |
| `notify`, `maintenance_branch` | Carried out by later milestones; recorded as pending until then |
| `allow` | Accept the change; auto-merge stays on |

Try your rules on real versions, check state, and lift a freeze:

```sh
forkkeeper rules test hashicorp/terraform v1.5.5 v1.6.0   # dry run, records nothing
forkkeeper status [owner/repo]                            # frozen repos and recent decisions
forkkeeper unfreeze --reason "upstream reverted" owner/repo
```

## Merging into your fork

Set `fork:` on a repo to have each new upstream release merged into it. Repos without a fork
are only watched and license-checked.

To create the fork itself, run `forkkeeper fork owner/repo`. It forks into the account of the
auth profile's token (or `--org`), waits until GitHub has copied the repo, and prints the config
to add. Forking with a fine-grained PAT needs **Administration: read & write** and repository
access set to "All repositories". Syncing doesn't need it, so you can remove it afterwards, or
just click "Fork" on GitHub instead.

```yaml
repos:
  - upstream: junegunn/fzf
    fork: your-user/fzf
    # stable_branch: main                 # default: the fork's default branch
    # staging_branch: forkkeeper/staging  # default; Fork Keeper owns and force-pushes it
    # merge_strategy: merge               # merge | patch-stack | merge-only
merge:
  auto_merge: true     # merge "auto" PRs once CI passes
  require_ci: true     # ...and only once at least one check has reported
```

For each release that isn't frozen:

1. **Fetch:** your fork's stable branch and the upstream tag are fetched into a bare cache under
   `<data_dir>/git`.
2. **Merge:** upstream is merged into stable with full history.
   - **On conflicts:** with the default `merge` strategy, your local patches are re-applied on top
     of upstream one at a time (a patch stack). Patches upstream already has are left out.
   - **The result:** it's still recorded as a merge of stable and upstream, so the PR merges
     cleanly and history is kept. The re-applied patches are pushed to `<staging>-patches` for review.
3. **Push:** the result goes to the staging branch. The stable branch is never pushed.
4. **Open a PR:** one PR from staging into stable is opened, or updated when a newer release
   replaces the previous one. Its body has the license check result, the merge policy and your
   local patches.
   - **Policy:** `approval_required` and `blocked` PRs are drafts, labeled `forkkeeper: needs
     approval` or `forkkeeper: blocked`. Labels are never removed, so a newer release can't clear
     an older concern.
5. **Check:** every `merge.check_interval` (5m), open PRs are checked. A PR merged or closed on
   GitHub is recorded. An `auto` PR is merged once its checks pass, unless it's a draft, labeled,
   or someone else pushed to staging.

If the merge and the patch stack both conflict, nothing is pushed. The conflicting files are
recorded (see `forkkeeper status`), and you merge that release by hand. Releases older than one
already synced (backports) are skipped.

```sh
forkkeeper sync --dry-run junegunn/fzf v0.74.5   # merge locally; push nothing, open no PR
forkkeeper sync junegunn/fzf v0.74.5             # what the service does for a new release
```

`sync` exits 3 when the release conflicts.

**Safety:**
- **Nothing from upstream runs:** merges use `git merge-tree` in a bare repository, so upstream
  files are never checked out.
- **Safe git config:** every git command ignores system and user config, disables hooks,
  fsmonitor and submodule recursion, and allows only HTTPS remotes.
- **Tokens stay hidden:** they're passed through the environment for that one command, never in
  arguments or saved remote URLs.

## Source backups

Releases are the backup points: Fork Keeper keeps the source of upstream releases, as git
history and tags. It never downloads release assets or binaries.

```yaml
archive:
  releases: minor            # major (X.0.0) | minor (X.Y.0, default) | all | none
  push_to_fork: true         # copy backups to the repo's fork (default)
  software_heritage: true    # ask Software Heritage to archive the upstream (default)
  # software_heritage_token: swh_token   # optional credential, for higher rate limits
  mirrors:                   # optional extra copies, e.g. on another host
    - url: https://codeberg.org/you/{name}.git   # {owner} and {name} are filled in
      token: codeberg_token  # credential name
      username: you          # Codeberg/Gitea: your user; GitLab: oauth2; GitHub: default
      refs: hidden           # hidden (default) | tags (for a dedicated backup repo)
```

- **What is kept:** each backed-up tag in a bare repository under `<data_dir>/archive`. Tags are
  only ever added: if upstream later moves or deletes a tag, the backup keeps the original commit.
- **Before 1.0:** `major` counts `0.Y.0` releases, since a minor bump is the breaking one there.
  Pre-releases are never backed up.
- **On a license change:** the version before it is always backed up as `forkkeeper/last-good`
  (with the `archive` action, or when a rule freezes the repo), even if it is a patch release.
- **Where copies go:** the fork and mirrors get every backed-up ref on each backup, so a failed
  push is retried next time. By default they go under `refs/forkkeeper/archive/`. These refs don't
  appear as tags or branches and never trigger CI, since a pushed upstream tag would otherwise
  run upstream's release workflows. To fetch them:
  `git fetch <fork> 'refs/forkkeeper/archive/*:refs/forkkeeper/archive/*'`.
- **Software Heritage:** asked at most once a day per upstream, and always on a license change.
- **Repo overrides:** each repo can set its own `archive:` (`releases`, `push_to_fork`,
  `software_heritage`, and extra `mirrors`).

Back up past releases, or list what is backed up:

```sh
forkkeeper archive --dry-run morluto/rea     # tags the policy would back up
forkkeeper archive morluto/rea               # back them up now
forkkeeper archive morluto/rea v1.2.3        # specific tags
forkkeeper archive list [owner/repo]         # backups, pushes, Software Heritage requests
```

## Default-branch early warning

Relicensing usually lands on the default branch weeks or months before a release ships it.
Terraform's BUSL commit was on main from August 2023, and v1.6.0 shipped in October. So each
poll also checks every upstream's default branch, without merging anything from it:

- **Cheap:** if the head hasn't moved, the check costs nothing (a 304). If it moved, Fork Keeper
  lists the root directory and reads the root license files and manifests, only when they changed.
- **Top-level only:** nested license files and headers on the default branch are ignored.
- **On a change:** the license rules run right away, so a freeze takes effect before the release
  arrives. Fork Keeper finds the last commit before the change (the first-parent commit before
  the one that touched the license files) and backs it up as `forkkeeper/last-open`. For Terraform
  this is `8a085b4`, the last MPL commit on main.
- **Turning it off:** set `watcher.main_branch: false`, or `main_branch: false` on a repo.

```sh
forkkeeper main --dry-run [owner/repo]   # check now; exit 3 if a change was found
```

## Credentials

Config values name credentials; they never contain the secrets. The `credentials.provider`
setting picks where they come from:

| Provider | Reads | Notes |
|---|---|---|
| `file` (default) | `<dir>/<name>` | `/run/secrets` under Compose, `$CREDENTIALS_DIRECTORY` under systemd. Re-read on every use; warns if group/world-readable |
| `env` | `FORKKEEPER_CRED_<NAME>` | e.g. `github_webhook_secret` -> `FORKKEEPER_CRED_GITHUB_WEBHOOK_SECRET` |
| `command` | stdout of `argv` | e.g. `[pass, show, "forkkeeper/{name}"]`, no shell. Cached until SIGHUP. Native installs only; the image has no tools |

GitHub auth profiles live under `github.auth`. A `type: app` profile authenticates as a GitHub App
(the installation is looked up per repo unless `installation_id` is set). A `type: pat` profile
uses a fine-grained personal access token. `github.repo_auth` maps `owner/name`, `owner/*` or `*`
to a profile; unmatched repos use `default`. See `config/forkkeeper.example.yaml`.

On startup and every 24 hours, each profile is validated with one GitHub call. The results are
logged: App permissions and webhook events, or the PAT's owner, scopes and expiry. You get a
warning if anything required is missing, or if a token expires within 14 days. To reload
credentials without a restart, send SIGHUP (`docker compose kill -s HUP forkkeeper`).

Verify a setup before starting the service:

```sh
docker compose run --rm forkkeeper check
```

## GitHub App setup

Create a GitHub App with these repository permissions:

| Permission | Access | Why |
|---|---|---|
| Contents | Read & write | Fetch and push the staging branch, merge PRs |
| Pull requests | Read & write | Open, update, label and merge sync PRs |
| Workflows | Read & write | Push upstream changes to `.github/workflows` (GitHub rejects them otherwise) |
| Checks, Commit statuses | Read | Wait for CI before auto-merging |
| Metadata | Read | Required by GitHub |

Install it on your forks, then save its private key as `secrets/github_app_private_key.pem` and
put its App ID in `.env`.

A fine-grained PAT for forks needs the same permissions, limited to those repos. A read-only PAT
can still poll upstreams, but pushes will fail. Map forks to the right profile with
`github.repo_auth`, then run `forkkeeper check`. It confirms each fork is reachable.

## GitHub webhook setup

In the GitHub App (or repo webhook) settings:

- **Payload URL:** `https://$FORKKEEPER_DOMAIN/webhooks/github`
- **Content type:** `application/json`
- **Secret:** the contents of `secrets/github_webhook_secret`
- **Events:** *Releases* and *Branch or tag creation*

## Layout

```
cmd/forkkeeper/        service entrypoint (serve | healthcheck | version)
config/                example config; your real config.yaml is gitignored
deploy/caddy/          reverse proxy: TLS, exposes only /webhooks/*
docs/                  specification
secrets/               mounted as /run/secrets; contents gitignored
Dockerfile             multi-stage Go build -> Alpine + git, nonroot
compose.yaml           production stack (forkkeeper + caddy)
compose.dev.yaml       local dev override (no proxy, debug logs)
```

## Deployment (Docker Compose)

```sh
cp .env.example .env                                   # set FORKKEEPER_DOMAIN, GitHub App IDs
cp config/forkkeeper.example.yaml config/forkkeeper.yaml
# put the GitHub App key and webhook secret in secrets/ (see secrets/README.md)
docker compose up -d --build
```

### Network exposure

| Port | Service | Exposure |
|---|---|---|
| 80/443 | Caddy | Public. Proxies only `/webhooks/*`; everything else returns 404 |
| 8080 | forkkeeper webhooks | Internal to the compose network, reached through Caddy |
| 8081 | forkkeeper API | Host only, bound to `API_BIND` (default `127.0.0.1`). Set it to a Tailscale or WireGuard IP for remote use |

### Hardening in place

- Minimal Alpine image (the merge engine needs `git`), running as non-root uid 65532
- Read-only root filesystem, all capabilities dropped, `no-new-privileges`
- Secrets mounted at runtime through compose `secrets:` and never baked into the image
- The Docker socket is never mounted. Upstream CI must run in a separate sandbox (spec §6.3)

## Development

```sh
docker compose -f compose.yaml -f compose.dev.yaml up --build forkkeeper
```
