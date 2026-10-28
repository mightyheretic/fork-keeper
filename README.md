# Fork Keeper

Self-hosted service that keeps maintained forks of the open-source apps you depend on.
When upstream publishes a release, Fork Keeper checks for license changes first, then merges
upstream into your fork on a staging branch, keeping history, and opens a PR. If a license
change breaks your rules, it applies the action you configured, such as freezing the fork,
archiving it or notifying you.

See [docs/spec.md](docs/spec.md) for the full specification.

**Status:** V1 milestone 1 done. The service receives and verifies GitHub webhooks, drops
duplicate deliveries and queues them durably in SQLite. Queued events are only logged until
the license check and merge engine land in later milestones.

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
