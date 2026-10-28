# Fork Keeper

Self-hosted service that keeps maintained forks of the open-source apps you depend on.
When upstream publishes a release, Fork Keeper checks for license changes first, then merges
upstream into your fork on a staging branch, keeping history, and opens a PR. If a license
change breaks your rules, it applies the action you configured, such as freezing the fork,
archiving it or notifying you.

See [docs/spec.md](docs/spec.md) for the full specification.

**Status:** V1 milestones 1–3 done. The service polls upstream repos for new releases (or
tags), receives and verifies GitHub webhooks, and queues each new release exactly once in
SQLite. It authenticates to GitHub as a GitHub App or with a fine-grained PAT, using pluggable
credential backends. Queued events are only logged until the license check and merge engine
land in later milestones.

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
