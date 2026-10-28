# Fork Keeper: Specification (Working Title)

Status: Draft v0.1
Date: 2026-10-28

## 1. Purpose

Keep a stable, maintained fork of each open-source app you depend on. When upstream publishes a major release, Fork Keeper runs license checks first, then merges upstream into your fork with commit history preserved wherever possible. If upstream changes license in a way you don't accept, it applies a configurable action (for example, freezing the fork at the last good tag).

## 2. Users and Deployment

- **User:** a single user (the owner). No multi-tenancy or billing.
- **Deployment:** self-hosted on an Arch-based Linux machine.
- **Exposure:** public internet, so upstream webhooks can reach the service.
- **Packaging:** Docker Compose or Podman stack, or a systemd service, with SQLite or Postgres for state.

## 3. Scope by Version

### V1 (must have)

1. Release watcher (GitHub only, behind a provider adapter interface)
2. Merge engine with history preservation
3. License transition rules (merged tripwire + policy engine)
4. Archival backup
5. Baseline plumbing: staging branch, CI run, PR instead of direct merge, basic notifications, audit log
6. Pluggable credential provider (GitHub App by default)
7. API-first core with a CLI

### V2 (should have)

- Web UI (responsive, installable as a PWA) for approvals: merge-conflict resolutions, license overrides, freeze reversals
- Health dashboard
- AI conflict assistant (human approval always required)
- Additional providers: GitLab, Bitbucket, Codeberg/Gitea/Forgejo, Azure DevOps, self-hosted git

### Later / parked

- Dependency license awareness (apply the same transition rules to the dependencies of each project)
- Native Android app (only if PWA proves insufficient)

## 4. Components

### 4.1 Release Watcher

- Primary trigger: webhooks (release and tag events).
- Safety net: slow reconciliation poll (every few hours) to catch dropped or missed webhooks. This matters most for the license tripwire.
- Provider adapter interface: each provider implements webhook verification, event normalization, repo operations and PR/MR creation. V1 ships GitHub only; the core never depends on GitHub-specific code.

### 4.2 Merge Engine

- Default: merge upstream with full history preserved.
- Fallback when conflicts are heavy: rebase, or a cherry-picked patch stack.
- Local patches are kept as a separate, labeled layer so they can be re-applied or reviewed independently.
- All merges happen on a staging branch. CI runs there. The result is opened as a PR, never pushed directly to the stable branch.

### 4.3 License Transition Rules (merged tripwire and policy)

The license gate compares the SPDX identifier, the LICENSE file and per-file headers between the previous and new version. Detected transitions are evaluated against user-defined rules.

**Rule shape:** `from-set -> to-set -> action(s)`

- **From:** one or many licenses (e.g., `MIT, Apache-2.0`).
- **To:** one or many licenses (e.g., `BSL-1.1, SSPL-1.0, Elastic-2.0`), or wildcards: `any not in allowlist`, `none/unknown`.
- **Actions** (one or more per rule):
  - Freeze fork at the last good tag
  - Block auto-merge
  - Require manual approval
  - Open a flagged PR
  - Notify
  - Snapshot to archive
  - Create a maintenance branch
- **Scope:** global, per repo, or per group of repos.
- **Priority:** the most specific rule wins; a configurable default rule applies when nothing matches.

**Example rules**

1. `MIT/Apache-2.0 -> BSL-1.1/SSPL-1.0` = freeze, archive, notify
2. `GPL-2.0 -> GPL-3.0` = require approval
3. `any -> unknown/none` = block auto-merge

**Edge cases to decide**

- Compound licenses (`MIT OR GPL`, `AND`): rules must handle licenses being added or removed, not only swapped.
- Per-file license changes versus the top-level LICENSE.
- Freeze semantics: stop syncing entirely, or keep cherry-picking pre-change security fixes?
- Freeze reversal: whether it is allowed and how it is done (approval plus audit entry).

### 4.4 Archival Backup

- Mirror each fork to a second provider and/or Software Heritage, so a deleted or tampered upstream cannot take your copy with it.
- Triggered on schedule and by the "snapshot to archive" rule action.

### 4.5 Approvals and Notifications

- Approvals are persistent objects with an ID, a context snapshot (diff, license change, rule that fired), and an expiry or reminder.
- V1: resolve through CLI and PR comments or labels.
- V2: web UI.
- Notification channel should support action links (ntfy, Gotify, or email with deep links).

### 4.6 Audit Log

Record every override, approval, rejection, freeze and unfreeze, with timestamp, rule that fired and context. These are license decisions you may need to justify later.

## 5. API-First Design

- Every action (approve, reject, override, freeze, unfreeze, add or edit rule) is an API call.
- The CLI and the V2 UI are both clients of the same API.
- A future native app would be another client.

## 6. Security

### 6.1 Webhook handling

- Verify every delivery (GitHub: HMAC-SHA256 signature header against the stored secret); reject unsigned or mismatched requests.
- Dedupe on the delivery ID (idempotency) so retries and replays do not trigger duplicate merges or checks.
- Acknowledge fast, queue the work, process in the background.
- Verification lives inside each provider adapter.

### 6.2 Public surface

- Only the webhook endpoint must be public.
- Keep the API and V2 UI on a separate path or host.
- Preferred: UI reachable only over Tailscale or WireGuard.
- If the UI is public: passkeys or OIDC with 2FA, rate limiting, short sessions.
- TLS via a reverse proxy (Caddy or Traefik).

### 6.3 Untrusted input

Webhook payloads and upstream repository contents are untrusted.

- Never run upstream code, hooks or build scripts on the host.
- Run CI on the staging branch in an isolated sandbox (container or systemd-nspawn), separate from any context holding secrets.

### 6.4 Credentials

**Pluggable credential provider** with backends:

- `file`: read from a path (e.g., `$CREDENTIALS_DIRECTORY`)
- `env`: environment variable
- `command`: run a command and read stdout (e.g., a password manager CLI)
- `vault`: OpenBao/Vault (later)

**Auth method (V1 default): GitHub App**

- Installation tokens expire after about an hour and are scoped only to installed repos.
- The only long-lived secret is the app private key.
- The App's webhook signing secret is stored the same way.
- Minimum permissions: Contents (read/write), Pull requests (read/write), Metadata (read), plus required webhook events.
- **PAT fallback:** fine-grained personal access token, limited to specific repos, for quick setups. A PAT cannot receive webhooks on its own; a separate webhook secret is still needed.

**Storage on Arch Linux (V1 default): systemd credentials bound to TPM2**

- Encrypt the App private key and webhook secret with `systemd-creds encrypt`, bound to the TPM2 (and optionally host key).
- Service uses `LoadCredentialEncrypted=`; systemd decrypts into a private in-memory directory exposed as `$CREDENTIALS_DIRECTORY`, readable only by the service.
- The app reads files through the `file` provider; no app changes needed.
- Caveat: TPM-bound credentials are lost if the TPM or motherboard is replaced. Keep an offline backup of the key; a new key can also be generated in GitHub.

**Upgrade paths:** SOPS + age (secrets in git without TPM dependency); OpenBao (many secrets or services).

**Rules**

- Never log secrets; redact in errors and debug output.
- On startup, check file permissions and warn if group/world-readable.
- Reload credentials on change or SIGHUP without restart.
- Validate on load (one GitHub call), report scopes and expiry, warn when expiry is near.
- Support per-repo or per-group credential mapping in config.

### 6.5 systemd hardening

- Dedicated unprivileged user or `DynamicUser=yes`
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`
- Restrict network egress to GitHub and the notification endpoint where possible
- If containerized, pass credentials via a systemd service or Podman quadlet, never baked into the image

## 7. V2 Details

### 7.1 Web UI (PWA)

- Responsive web app installable on desktop and Android (one codebase, push notifications, no APK upkeep).
- Screens: approval queue, conflict resolution/diff review, license override approvals, freeze reversal, rule editor.
- Desktop-first for diff and conflict review; mobile-friendly for quick approvals.

### 7.2 Health Dashboard

Per fork: commits behind upstream, local patch count, last license check, last sync, maintainer activity (early warning of abandonment).

### 7.3 Conflict Assistant

AI explains conflicts and suggests resolutions; a human must approve before anything is applied. Plugs into the approval UI.

## 8. Existing Tools and Positioning

- Dependabot and Renovate: dependency updates, not forks.
- GitHub "Sync fork" and the Pull bot: fork syncing, no license checks.
- FOSSA and ScanCode: license scanning, no fork management.

Fork Keeper combines fork syncing, license-aware policy and archival in one self-hosted tool.

## 9. Open Questions

1. Freeze semantics: full stop, or continued security cherry-picks?
2. Compound-license handling in rules (`OR`, `AND`, added/removed licenses).
3. Per-file license change detection: how deep, and how to avoid noise.
4. V2 UI exposure: public with strong auth, or VPN-only?
5. State store: SQLite or Postgres?
6. Where CI sandbox runs (container vs systemd-nspawn) and how results are reported back.
7. Reconciliation poll interval.

## 10. Suggested V1 Milestones

1. Provider adapter interface, GitHub adapter, webhook receiver with verification and idempotency
2. Credential provider (file/env/command) and GitHub App auth
3. License diff detection (SPDX, LICENSE file, headers)
4. Rule engine with scopes, priorities and actions
5. Merge engine on staging branch with PR creation
6. Archival mirror and Software Heritage snapshot
7. CLI for approvals and rule management, audit log, notifications
8. Reconciliation poll, systemd unit and hardening, packaging
