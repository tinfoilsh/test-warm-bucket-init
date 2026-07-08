# test-warm-bucket-init

A warm-start proxy for [tinfoil-buckets-sidecar](https://github.com/tinfoilsh/tinfoil-buckets-sidecar). The sidecar starts **uninitialized** — no AWS credentials in the container env — and waits for a `POST /__configure` to inject them at runtime, then spawns the sidecar JAR and reverse-proxies traffic to it.

This lets the sidecar run inside a Tinfoil enclave without baking AWS secrets into the container definition or fetching them at boot. Credentials arrive at the application layer when a user connects.

## Lifecycle

Follows the `warm → active → killed` pattern from [code-execution-environment](https://github.com/tinfoilsh/code-execution-environment):

| State | Sidecar | `/__configure` | Other requests |
|-------|---------|-----------------|-----------------|
| **warm** | not started | starts sidecar, → active | 503 |
| **active** | running on `:9001` | idempotent (same creds = no-op, different creds = restart) | proxied to sidecar |
| **killed** | stopped | 410 | 410 |

If the sidecar process dies unexpectedly while active, the proxy transitions back to **warm** so it can be reconfigured.

## API

### `POST /__configure`

Injects AWS credentials and starts (or restarts) the sidecar.

```json
{
  "access_key_id": "AKIA...",
  "secret_access_key": "...",
  "session_token": "..."  // optional, for STS temporary creds
}
```

Idempotent: calling with the same creds while active returns 200 without restarting. Calling with different creds kills the old sidecar and starts a new one.

### `GET /__status`

```json
{"status": "warm"}
```

### `POST /__kill`

Stops the sidecar and transitions to `killed`. Irreversible.

### `GET /health`

Always returns 200 (the proxy is alive). Used by cvmimage healthchecks.

### All other paths

Reverse-proxied to the sidecar on `:9001` when active.

## How it works

```
container starts
  └─ sidecar-init (PID 1, listens on :9000, state=warm)
       │
  POST /__configure {creds}
       │
       ├─ spawns: java -jar /app/app.jar  (env: AWS_* creds, PORT=9001, inherited AWS_REGION/MULTITENANT)
       ├─ waits for :9001 to accept TCP
       └─ state=active, reverse-proxies :9000 → :9001
```

The sidecar JAR is unchanged — it still reads AWS creds from env vars at startup. The proxy just delays `java -jar` until creds are available.

## Docker

Multi-stage build layers the Go binary on top of the published sidecar image:

```dockerfile
FROM ghcr.io/tinfoilsh/tinfoil-buckets-sidecar@sha256:... AS sidecar
COPY --from=build /sidecar-init /app/sidecar-init
ENTRYPOINT ["/app/sidecar-init"]
```

The resulting image has both the sidecar JAR (`/app/app.jar`) and the init proxy (`/app/sidecar-init`). The proxy is PID 1; the sidecar is a child process spawned on configure.

## tinfoil-config.yml usage

The `secrets:` block is removed from the buckets container — AWS creds come at runtime:

```yaml
containers:
  - name: "buckets"
    image: "ghcr.io/tinfoilsh/test-warm-bucket-init:latest"
    restart: always
    networks: [web]
    env:
      - PORT: "9000"
      - AWS_REGION: "us-east-2"
      - MULTITENANT: "true"
    # No secrets: block — AWS creds injected via POST /__configure at runtime
```

The sync enclave calls `POST http://buckets:9000/__configure` with the user's AWS credentials after authenticating them, before first bucket use.
