# secrets/

Everything in this folder except this README is gitignored. Compose mounts these files
into the container at `/run/secrets/`, where the `file` credential provider reads them.

| File | Contents |
|---|---|
| `github_app_private_key.pem` | GitHub App private key (the only long-lived secret) |
| `github_webhook_secret` | GitHub App webhook signing secret |

Lock down permissions; Fork Keeper warns at startup if these files are group- or world-readable:

```sh
chmod 700 secrets
chmod 600 secrets/*
```

The container runs as uid 65532. If it can't read the files, give that uid ownership
(`sudo chown 65532:65532 secrets/*`) and keep mode 600, rather than loosening permissions.

Keep an offline backup of the private key. If it is lost, generate a new one in the GitHub App settings.
