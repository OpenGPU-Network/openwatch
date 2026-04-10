# OpenWatch — Product Requirements Document

**Version:** 1.0  
**Status:** Active  
**Language:** Go  
**Last Updated:** April 2026

---

## 1. Overview

OpenWatch is a lightweight, self-hosted Docker container auto-update daemon written in Go. It monitors running containers, detects new image versions by comparing digests, and automatically recreates containers with updated images — all without manual intervention.

OpenWatch is the spiritual successor to `containrrr/watchtower`, which was archived in December 2025 due to incompatibility with Docker Engine 29+ (embedded Docker SDK was pinned to API v1.25; modern Docker requires API ≥ v1.44). OpenWatch is built from scratch using the modern Docker Go SDK with `WithAPIVersionNegotiation()`, making it compatible with Docker Engine 19.03+ through 29+.

---

## 2. Goals

- Drop-in replacement for Watchtower with zero config migration
- Compatible with all Docker Engine versions from 19.03 to latest (via API negotiation)
- Lightweight: single static Go binary, minimal resource usage
- Auto-update containers with optional rollback on healthcheck failure
- Label-based per-container control
- Notifications via webhook, Telegram, Slack
- Production-safe defaults (digest-based comparison, graceful shutdown, cleanup)

---

## 3. Non-Goals

- Kubernetes / Docker Swarm support (out of scope for v1)
- Web dashboard / UI (CLI + logs only in v1)
- Multi-host / remote Docker management (out of scope for v1)
- OpenGPU / RelayGPU integration (intentionally excluded)
- Built-in registry mirror / caching proxy

---

## 4. Tech Stack

| Component | Choice | Reason |
|---|---|---|
| Language | Go 1.22+ | Docker ecosystem standard, static binary, great concurrency |
| Docker SDK | `github.com/docker/docker` (latest) | Official SDK, API negotiation built-in |
| Scheduler | `github.com/robfig/cron/v3` | Battle-tested cron for Go |
| Config | `github.com/spf13/viper` | Env vars + YAML, widely used |
| Notifications | `github.com/containrrr/shoutrrr` | Multi-provider notification library |
| Logging | `github.com/rs/zerolog` | Structured, leveled, zero-alloc |
| Testing | `testing` + `testify` | Standard Go testing |
| Container | `gcr.io/distroless/static` | Minimal attack surface |

---

## 5. Architecture

```
openwatch/
├── cmd/
│   └── openwatch/
│       └── main.go               # Entry point, DI wiring
├── internal/
│   ├── docker/
│   │   ├── client.go             # Docker Engine client (WithAPIVersionNegotiation)
│   │   ├── container.go          # List, inspect, stop, start, recreate
│   │   └── image.go              # Pull, digest compare, prune old images
│   ├── registry/
│   │   ├── resolver.go           # Parse image ref → registry URL + repo + tag
│   │   ├── auth.go               # Auth: Docker config.json + env vars + anonymous
│   │   └── digest.go             # Fetch remote manifest digest via Registry HTTP API v2
│   ├── updater/
│   │   ├── watcher.go            # Main poll loop: list containers → check → update
│   │   ├── strategy.go           # Per-container update decision logic
│   │   └── rollback.go           # Healthcheck monitor → rollback to previous image
│   ├── notify/
│   │   ├── notifier.go           # Notifier interface
│   │   └── shoutrrr.go           # shoutrrr-backed multi-provider implementation
│   └── config/
│       └── config.go             # Load config from env + openwatch.yaml
├── Dockerfile
├── docker-compose.yml
├── openwatch.yaml.example
└── CLAUDE.md
```

---

## 6. Core Concepts

### 6.1 Update Detection

OpenWatch compares the **SHA256 digest** of the running container's image against the **remote manifest digest** fetched directly from the registry. This is more reliable than tag-based comparison because:

- A tag (e.g., `latest`) can point to different digests over time
- Digest comparison detects actual image content changes, not just tag updates
- Works for both `:latest` and pinned tags (e.g., `:1.2.3`)

Flow:
```
running container
  → inspect → get current image ID (sha256)
  → resolve registry + repo + tag from image ref
  → fetch remote manifest digest via Registry API v2
  → compare: local digest ≠ remote digest → update
```

### 6.2 Container Recreate

When an update is detected:
1. Pull new image
2. Inspect existing container → capture full config (env, volumes, ports, networks, labels, restart policy, etc.)
3. Stop existing container (SIGTERM → wait → SIGKILL if needed)
4. Remove old container
5. Create new container with identical config but new image
6. Start new container
7. (Optional) wait for healthcheck → rollback if unhealthy

### 6.3 API Version Negotiation

```go
cli, err := client.NewClientWithOpts(
    client.FromEnv,
    client.WithAPIVersionNegotiation(), // auto-negotiate with daemon
)
```

This single line ensures compatibility from Docker Engine 19.03 (API 1.40) through latest.
Minimum supported: Docker Engine 19.03 / API 1.40.

---

## 7. Configuration

### 7.1 Environment Variables (primary method)

| Variable | Default | Description |
|---|---|---|
| `OPENWATCH_INTERVAL` | `86400` | Poll interval in seconds (default: 24h) |
| `OPENWATCH_SCHEDULE` | `""` | Cron expression (overrides INTERVAL if set) |
| `OPENWATCH_CLEANUP` | `false` | Remove old images after update |
| `OPENWATCH_INCLUDE_STOPPED` | `false` | Also monitor stopped containers |
| `OPENWATCH_LABEL_ENABLE` | `false` | Require `openwatch.enable=true` label to opt-in |
| `OPENWATCH_ROLLBACK_ON_FAILURE` | `false` | Auto-rollback if healthcheck fails post-update |
| `OPENWATCH_HEALTHCHECK_TIMEOUT` | `30` | Seconds to wait for healthcheck after update |
| `OPENWATCH_STOP_TIMEOUT` | `10` | Seconds to wait for graceful stop before SIGKILL |
| `OPENWATCH_NOTIFY_URL` | `""` | shoutrrr notification URL (e.g. `telegram://token@telegram?channels=chatid`) |
| `OPENWATCH_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |
| `OPENWATCH_LOG_FORMAT` | `text` | Log format: text, json |
| `OPENWATCH_HTTP_API` | `false` | Enable HTTP API on port 8080 |
| `DOCKER_HOST` | (from env) | Docker daemon socket / TCP address |

### 7.2 openwatch.yaml (optional, overrides env)

```yaml
interval: 3600           # seconds, or use schedule
schedule: "0 4 * * *"   # cron: run at 4 AM daily (overrides interval)
cleanup: true
label_enable: false
rollback_on_failure: true
healthcheck_timeout: 60
stop_timeout: 10
log:
  level: info
  format: text
notify:
  urls:
    - "telegram://token@telegram?channels=chatid"
    - "slack://token@channel"
```

### 7.3 Container Labels

Labels are set on individual containers to override global behavior:

| Label | Values | Description |
|---|---|---|
| `openwatch.enable` | `true` / `false` | Force include or exclude this container |
| `openwatch.rollback` | `true` / `false` | Override rollback setting for this container |
| `openwatch.cleanup` | `true` / `false` | Override cleanup setting for this container |
| `openwatch.notify_only` | `true` | Only send notification, do not update |
| `openwatch.stop_timeout` | integer (seconds) | Override stop timeout for this container |
| `openwatch.depends_on` | container name | Update this container only after named container |

Example in `docker-compose.yml`:
```yaml
services:
  myapp:
    image: myrepo/myapp:latest
    labels:
      openwatch.enable: "true"
      openwatch.rollback: "true"
      openwatch.cleanup: "true"

  database:
    image: postgres:16
    labels:
      openwatch.enable: "false"   # never auto-update the database

  staging:
    image: myrepo/myapp:staging
    labels:
      openwatch.notify_only: "true"  # just tell me, don't update
```

---

## 8. Deployment

### 8.1 Docker Run
```bash
docker run -d \
  --name openwatch \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e OPENWATCH_INTERVAL=3600 \
  -e OPENWATCH_CLEANUP=true \
  ghcr.io/openwatch/openwatch:latest
```

### 8.2 Docker Compose
```yaml
services:
  openwatch:
    image: ghcr.io/openwatch/openwatch:latest
    container_name: openwatch
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./openwatch.yaml:/etc/openwatch/openwatch.yaml:ro  # optional
    environment:
      - OPENWATCH_SCHEDULE=0 4 * * *
      - OPENWATCH_CLEANUP=true
      - OPENWATCH_ROLLBACK_ON_FAILURE=true
      - OPENWATCH_NOTIFY_URL=telegram://token@telegram?channels=chatid
```

### 8.3 Docker Socket via TCP
```bash
docker run -d \
  --name openwatch \
  -e DOCKER_HOST=tcp://remote-host:2376 \
  ghcr.io/openwatch/openwatch:latest
```

---

## 9. Registry Authentication

Priority order (highest to lowest):
1. `DOCKER_CONFIG` env var pointing to a config.json directory
2. `~/.docker/config.json` mounted into the container
3. Per-registry env vars: `REPO_USER` / `REPO_PASS` (e.g. `GHCR_USER` / `GHCR_PASS`)
4. Anonymous / public access

Supported registries:
- Docker Hub (`docker.io`, `registry-1.docker.io`)
- GitHub Container Registry (`ghcr.io`)
- Google Container Registry (`gcr.io`)
- Amazon ECR (`*.dkr.ecr.*.amazonaws.com`)
- Generic private registry (any hostname)

---

## 10. Rollback

When `rollback_on_failure: true` (or `openwatch.rollback: "true"` label):

1. After container starts with new image, OpenWatch monitors healthcheck status
2. Waits up to `healthcheck_timeout` seconds
3. If container status becomes `unhealthy` or exits unexpectedly:
   - Stop the new container
   - Recreate with the **previous image digest** (stored before update)
   - Send rollback notification
4. If no healthcheck is defined on the container, rollback is skipped (log a warning)

---

## 11. Notifications

Uses `shoutrrr` library. Single `OPENWATCH_NOTIFY_URL` env var (or list in yaml). Any shoutrrr-compatible URL works out of the box:

- `telegram://token@telegram?channels=chatid`
- `slack://token@channel`
- `discord://token@id`
- `generic://webhook-url` (POST JSON)
- `gotify://host/token`
- `ntfy://host/topic`

Notification events:
- `UPDATE_STARTED` — pulling new image
- `UPDATE_SUCCESS` — container recreated successfully
- `UPDATE_FAILED` — update failed with error
- `ROLLBACK_TRIGGERED` — healthcheck failed, rolling back
- `ROLLBACK_SUCCESS` — rolled back successfully
- `ROLLBACK_FAILED` — rollback also failed (critical)

---

## 12. HTTP API (optional, v1.1+)

When `OPENWATCH_HTTP_API=true`, exposes on port `8080`:

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health probe (always 200) |
| `GET` | `/metrics` | Prometheus metrics |
| `GET` | `/api/v1/containers` | List monitored containers + status |
| `POST` | `/api/v1/update` | Trigger immediate update check |
| `POST` | `/api/v1/update/:name` | Trigger update for specific container |

---

## 13. Development Phases

### Phase 1 — Core MVP
- [ ] Project scaffold: `go mod init`, directory structure
- [ ] Docker client (`internal/docker/client.go`) with `WithAPIVersionNegotiation`
- [ ] Container lister + image digest reader (`internal/docker/container.go`)
- [ ] Registry manifest digest fetcher (`internal/registry/digest.go`)
- [ ] Auth resolver (`internal/registry/auth.go`)
- [ ] Image pull + container recreate with identical config (`internal/docker/image.go`, `container.go`)
- [ ] Interval scheduler (`internal/updater/watcher.go`)
- [ ] Config loader: env vars (`internal/config/config.go`)
- [ ] Structured logging with zerolog
- [ ] Single binary Dockerfile (distroless)
- [ ] Basic `docker-compose.yml` example

### Phase 2 — Safety Layer
- [ ] Post-update healthcheck monitor
- [ ] Rollback on failure (`internal/updater/rollback.go`)
- [ ] Graceful stop (SIGTERM → timeout → SIGKILL)
- [ ] Label-based per-container control
- [ ] `openwatch.yaml` config file support (via viper)
- [ ] Cron schedule support

### Phase 3 — Notifications
- [ ] shoutrrr notifier integration (`internal/notify/`)
- [ ] Notification events: update started/success/failed/rollback
- [ ] Per-container `notify_only` label support

### Phase 4 — Production Polish
- [ ] Private registry auth (Docker Hub, GHCR, ECR, generic)
- [ ] `OPENWATCH_LABEL_ENABLE` opt-in mode
- [ ] Prometheus metrics endpoint (`/metrics`)
- [ ] HTTP API: `/health`, `/api/v1/containers`, `/api/v1/update`
- [ ] Multi-arch Docker build (linux/amd64, linux/arm64)
- [ ] GitHub Actions CI: test + build + push to GHCR
- [ ] README + docs

---

## 14. Key Implementation Notes

### Container Recreate — Config Capture

When recreating a container, the following config must be preserved:
- `HostConfig`: Binds (volumes), PortBindings, RestartPolicy, NetworkMode, CapAdd/Drop, Devices, ShmSize, LogConfig, ExtraHosts, PidMode, Privileged, ReadonlyRootfs, SecurityOpt, Ulimits, Resources
- `Config`: Env, Cmd, Entrypoint, WorkingDir, User, Labels, ExposedPorts, Volumes, StopSignal, StopTimeout
- `NetworkingConfig`: EndpointsConfig (all connected networks + aliases + IPs)
- Container name (must remove old container before creating with same name)

### Image Digest Comparison

```
local:  docker inspect container → ImageID (sha256:abc...)
remote: GET /v2/{repo}/manifests/{tag} with Accept: application/vnd.oci.image.manifest.v1+json
        → response Docker-Content-Digest header → sha256:xyz...
update if local ≠ remote
```

Important: Docker Hub uses two levels of indirection (manifest list → platform manifest). Always resolve to the platform-specific manifest for the current architecture.

### Watchtower Migration

For users migrating from Watchtower:
- Environment variables are intentionally similar (`WATCHTOWER_*` → `OPENWATCH_*`)
- Label key changes: `com.centurylinklabs.watchtower.*` → `openwatch.*`
- Docker socket mount is identical
- No database, no state file — stateless by design

---

## 15. Out of Scope (Future Consideration)

- Web UI / dashboard
- Multi-host agent system
- Kubernetes CRD / operator
- Semantic versioning-aware update filtering (major/minor/patch thresholds)
- Changelogs / release notes fetching
- Vulnerability scanning before update
- Scheduled maintenance windows per container