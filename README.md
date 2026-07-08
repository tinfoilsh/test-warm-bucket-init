# test-warm-bucket-init

A warm-start proxy for [tinfoil-buckets-sidecar](https://github.com/tinfoilsh/tinfoil-buckets-sidecar). The sidecar starts **uninitialized** — no AWS credentials in the container env — and waits for a `POST /__configure` to inject them at runtime, then starts serving.

This lets the sidecar run inside a Tinfoil enclave without baking AWS secrets into the container definition or fetching them at boot. Credentials arrive at the application layer when a user connects.

## Architecture

Two containers, matching the [code-execution-environment](https://github.com/tinfoilsh/code-execution-environment) pattern:

```
┌─────────────────────────────────────────────────────┐
│  Enclave                                             │
│                                                      │
│  ┌──────────────┐       ┌──────────────────────┐    │
│  │  main        │       │  buckets              │    │
│  │  (init-proxy)│       │  (sidecar JAR)        │    │
│  │              │       │                       │    │
│  │  :9000       │──────▶│  :9001                │    │
│  │  /__configure│       │  entrypoint waits    │    │
│  │  reverse proxy│      │  for /run/init/creds  │    │
│  │              │       │  .env, then execs    │    │
│  └──────┬───────┘       └────────▲─────────────┘    │
│         │                        │                    │
│         │  writes creds.env      │  sources it        │
│         └────────────────────────┘                    │
│              shared volume: initvol                   │
└──────────────────────────────────────────────────────┘
```

- **main** (this repo): a Go binary that's PID 1 in the `main` container. Listens on `:9000`, handles `/__configure`, reverse-proxies all other traffic to `buckets:9001`.
- **buckets**: the published sidecar image, **unchanged**. Its `entrypoint` is overridden in the config to a shell script that blocks until `/run/init/creds.env` appears on the shared volume, sources it, then `exec java -jar /app/app.jar`.

The sidecar JAR is completely untouched — it still reads AWS creds from env vars at startup. The entrypoint override just delays `java -jar` until the proxy writes the creds file.

## Lifecycle

Follows the `warm → active → killed` pattern from code-execution-environment:

| State | Buckets container | `/__configure` | Other requests |
|-------|-------------------|-----------------|-----------------|
| **warm** | waiting for creds file | writes creds, waits for :9001, → active | 503 |
| **active** | running | idempotent (same creds = no-op) | proxied to buckets:9001 |
| **killed** | n/a | 410 | 410 |

## API

### `POST /__configure`

Injects AWS credentials. Writes them to the shared volume as a shell-sourceable env file, then waits for the buckets container to accept connections on `:9001`.

```json
{
  "access_key_id": "AKIA...",
  "secret_access_key": "...",
  "session_token": "..."
}
```

`session_token` is optional (for STS temporary creds). Idempotent: calling with the same creds while active returns 200 without restarting.

### `GET /__status`

```json
{"status": "warm"}
```

### `POST /__kill`

Removes the creds file and transitions to `killed`. Irreversible.

### `GET /health`

Always returns 200 (the proxy is alive). Used by cvmimage healthchecks.

### All other paths

Reverse-proxied to `buckets:9001` when active.

## tinfoil-config.yml

```yaml
containers:
  - name: "main"
    image: "ghcr.io/tinfoilsh/test-warm-bucket-init@sha256:..."
    volumes:
      - "initvol:/run/init"

  - name: "buckets"
    image: "ghcr.io/tinfoilsh/tinfoil-buckets-sidecar@sha256:..."
    entrypoint: ["/bin/sh", "-c"]
    command:
      - |
        while [ ! -f /run/init/creds.env ]; do sleep 0.5; done
        . /run/init/creds.env
        exec java -jar /app/app.jar
    env:
      - PORT: "9001"
      - AWS_REGION: "us-east-2"
      - MULTITENANT: "true"
    volumes:
      - "initvol:/run/init"
```

The `buckets` container has no `secrets:` block — AWS creds arrive at runtime via `/__configure`. The sync enclave calls `POST http://main:9000/__configure` after authenticating the user, before first bucket use.
