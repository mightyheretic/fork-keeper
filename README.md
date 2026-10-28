# Fork Keeper

Self-hosted service that keeps maintained forks of the open-source apps you depend on.
When upstream publishes a release, Fork Keeper checks for license changes first, then merges
upstream into your fork on a staging branch, keeping history, and opens a PR. If a license
change breaks your rules, it applies the action you configured, such as freezing the fork,
archiving it or notifying you.

See [docs/spec.md](docs/spec.md) for the full specification.

**Status:** V1 milestones 1–5 done.
- **Watching:** the service polls upstream repos for new releases (or tags) and receives verified GitHub webhooks. Each new release is queued once in SQLite.
- **License checks:** for each new release, it compares the licenses against the previous stable version.
- **Rules:** your license rules decide what happens. A repo can be frozen at its last good version, and each version gets a merge policy for the merge engine.
- **GitHub access:** it authenticates as a GitHub App or with a fine-grained PAT, using pluggable credential backends.

Merging, archiving and notifications land in later milestones. Until then those actions are
recorded as pending.

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
- **Level:** `level: any` also looks at nested license files and file headers, not just the top level.
- **Which rule wins:** the most specific rule (repo > group > global), then the highest `priority`, then the earliest in the file.
- **New repos:** a repo's first version is checked against the allowlist.

| Action | Effect |
|---|---|
| `freeze` | The fork stays at the last version that passed. Takes effect immediately |
| `block_auto_merge` | The PR is opened but never merged automatically |
| `require_approval` | The PR merges only after you approve it |
| `flag_pr`, `notify`, `archive`, `maintenance_branch` | Carried out by later milestones; recorded as pending until then |
| `allow` | Accept the change; auto-merge stays on |

Try your rules on real versions, check state, and lift a freeze:

```sh
forkkeeper rules test hashicorp/terraform v1.5.5 v1.6.0   # dry run, records nothing
forkkeeper status [owner/repo]                            # frozen repos and recent decisions
forkkeeper unfreeze --reason "upstream reverted" owner/repo
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

Create a GitHub App with these repository permissions: **Contents** read & write, **Pull requests**
read & write, and **Metadata** read. Install it on your forks, then save its private key as
`secrets/github_app_private_key.pem` and put its App ID in `.env`.

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
Dockerfile             multi-stage Go build -> distroless nonroot image
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

- Distroless image with no shell, running as non-root uid 65532
- Read-only root filesystem, all capabilities dropped, `no-new-privileges`
- Secrets mounted at runtime through compose `secrets:` and never baked into the image
- The Docker socket is never mounted. Upstream CI must run in a separate sandbox (spec §6.3)

## Development

```sh
docker compose -f compose.yaml -f compose.dev.yaml up --build forkkeeper
```
