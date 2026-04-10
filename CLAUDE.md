# OpenWatch — CLAUDE.md

This file is the primary reference for Claude Code when working on this project.
Read PRD.md for full product requirements before making any implementation decisions.

---

## Project Summary

OpenWatch is a Docker container auto-update daemon written in Go.
It replaces the abandoned `containrrr/watchtower` project.
Core differentiator: uses Docker SDK with `WithAPIVersionNegotiation()` — compatible with Docker Engine 19.03 through 29+.

---

## Tech Stack

- **Language:** Go 1.22+
- **Docker SDK:** `github.com/docker/docker` (latest) + `github.com/docker/distribution`
- **Scheduler:** `github.com/robfig/cron/v3`
- **Config:** `github.com/spf13/viper`
- **Notifications:** `github.com/containrrr/shoutrrr`
- **Logging:** `github.com/rs/zerolog`
- **Testing:** `testing` + `github.com/stretchr/testify`
- **Container base:** `gcr.io/distroless/static`

---

## Directory Structure

```
openwatch/
├── cmd/openwatch/main.go
├── internal/
│   ├── docker/
│   │   ├── client.go       # Docker Engine client setup
│   │   ├── container.go    # Container operations
│   │   └── image.go        # Image pull, cleanup
│   ├── registry/
│   │   ├── resolver.go     # Image ref parsing
│   │   ├── auth.go         # Registry authentication
│   │   └── digest.go       # Remote manifest digest fetch
│   ├── updater/
│   │   ├── watcher.go      # Main poll loop
│   │   ├── strategy.go     # Update decision per container
│   │   └── rollback.go     # Healthcheck + rollback
│   ├── notify/
│   │   ├── notifier.go     # Interface
│   │   └── shoutrrr.go     # shoutrrr implementation
│   └── config/
│       └── config.go       # Config loader
├── Dockerfile
├── docker-compose.yml
├── openwatch.yaml.example
├── PRD.md
└── CLAUDE.md
```

---

## Critical Implementation Rules

### Docker Client — always use API negotiation
```go
cli, err := client.NewClientWithOpts(
    client.FromEnv,
    client.WithAPIVersionNegotiation(),
)
```
Never hardcode or set a specific API version. This is the core fix over Watchtower.

### Container Recreate — preserve full config
When recreating a container after image update, capture and restore:
- `HostConfig` (volumes, ports, restart policy, capabilities, resources, log config)
- `Config` (env, cmd, entrypoint, workdir, user, labels, exposed ports)
- `NetworkingConfig` (all networks + aliases)
- Container name

### Digest Comparison Flow
```
inspect running container → get local image SHA256
→ parse image ref → resolve registry + repo + tag
→ fetch remote manifest via Registry API v2
→ extract Docker-Content-Digest header
→ compare → if different → update
```

Handle manifest lists (multi-arch): resolve to platform-specific manifest using current arch.

### Error Handling
- Never crash the daemon on a single container update failure
- Log error, skip container, continue to next
- Only fatal errors (Docker socket unavailable) should exit

### Config Priority (highest → lowest)
1. Environment variables
2. `openwatch.yaml` / `/etc/openwatch/openwatch.yaml`
3. Default values

---

## Container Labels

```
openwatch.enable          true/false   include or exclude
openwatch.rollback        true/false   override rollback setting
openwatch.cleanup         true/false   override cleanup setting
openwatch.notify_only     true         notify but don't update
openwatch.stop_timeout    int (secs)   override stop timeout
openwatch.depends_on      string       update after this container
```

---

## Environment Variables

```
OPENWATCH_INTERVAL              default: 86400 (seconds)
OPENWATCH_SCHEDULE              cron expression (overrides INTERVAL)
OPENWATCH_CLEANUP               default: false
OPENWATCH_INCLUDE_STOPPED       default: false
OPENWATCH_LABEL_ENABLE          default: false (false = watch all containers)
OPENWATCH_ROLLBACK_ON_FAILURE   default: false
OPENWATCH_HEALTHCHECK_TIMEOUT   default: 30 (seconds)
OPENWATCH_STOP_TIMEOUT          default: 10 (seconds)
OPENWATCH_NOTIFY_URL            shoutrrr URL
OPENWATCH_LOG_LEVEL             debug/info/warn/error (default: info)
OPENWATCH_LOG_FORMAT            text/json (default: text)
OPENWATCH_HTTP_API              default: false
DOCKER_HOST                     (standard Docker env var)
```

---

## Development Phases

Work through phases in order. Do not start Phase 2 work until Phase 1 is complete and tested.

**Phase 1 — Core MVP** (start here)
1. `go mod init github.com/openwatch/openwatch`
2. Docker client with API negotiation
3. List running containers + read local image digest
4. Fetch remote manifest digest from registry
5. Pull new image if digest differs
6. Stop → remove → recreate container with full config preserved
7. Interval scheduler (simple ticker, no cron yet)
8. Env var config loading
9. Zerolog structured logging
10. Dockerfile (multi-stage, distroless output)

**Phase 2 — Safety**
- Post-update healthcheck monitoring
- Rollback to previous image on failure
- Graceful stop (SIGTERM + configurable timeout)
- Label-based per-container control
- YAML config file support
- Cron schedule support

**Phase 3 — Notifications**
- shoutrrr notifier
- Events: UPDATE_STARTED, UPDATE_SUCCESS, UPDATE_FAILED, ROLLBACK_TRIGGERED, ROLLBACK_SUCCESS, ROLLBACK_FAILED

**Phase 4 — Production Polish**
- Private registry auth (Docker Hub, GHCR, ECR, generic)
- Prometheus metrics
- HTTP API (/health, /metrics, /api/v1/containers, /api/v1/update)
- Multi-arch Docker build (amd64 + arm64)
- GitHub Actions CI

---

## Testing Strategy

- Unit test digest comparison logic independently
- Unit test config loading (env + yaml precedence)
- Unit test label parsing
- Integration tests: use `github.com/testcontainers/testcontainers-go` to spin up a real Docker daemon and test actual update flow
- Table-driven tests for registry URL resolution

---

## Dockerfile Pattern

```dockerfile
# Build stage
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o openwatch ./cmd/openwatch

# Final stage — distroless
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/openwatch /openwatch
ENTRYPOINT ["/openwatch"]
```

---

## What NOT to Do

- Do not use `client.WithVersion("x.xx")` — always use negotiation
- Do not store state in files — OpenWatch is stateless (only in-memory previous digest for rollback)
- Do not update the `openwatch` container itself (skip self by container name detection)
- Do not expose the Docker socket over the network without TLS
- Do not pull images before confirming digest difference — avoid unnecessary bandwidth