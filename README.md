# OpenWatch

A lightweight Docker container auto-update daemon written in Go — a modern, API-negotiating replacement for Watchtower.

[![Go Version](https://img.shields.io/github/go-mod/go-version/OpenGPU-Network/openwatch)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](#license)
[![Docker Hub](https://img.shields.io/docker/pulls/opengpunetwork/openwatch)](https://hub.docker.com/r/opengpunetwork/openwatch)

---

## Why OpenWatch?

[Watchtower](https://github.com/containrrr/watchtower) was the go-to container auto-updater for years, but it was archived in December 2025 because its embedded Docker SDK pinned API version `v1.25` and Docker Engine 29+ now requires `v1.44` or newer. That single pin made Watchtower unable to talk to current Docker daemons, leaving thousands of self-hosted deployments stranded on older engines. Watchtower served its users well for the better part of a decade; it just stopped being compatible with the current Docker ABI.

OpenWatch is a ground-up rewrite that uses the official Docker Go SDK with `client.WithAPIVersionNegotiation()`, so the daemon negotiates the API version with whatever Docker Engine it happens to find. It is compatible with Docker Engine **19.03 through 29+** from a single binary. Everything else — digest-based update detection, container recreate with full config preservation, rollback on healthcheck failure, label-based opt-in, shoutrrr notifications — is built around that compatibility baseline.

---

## Quick start

### Docker run

```bash
docker run -d \
  --name openwatch \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e OPENWATCH_INTERVAL=3600 \
  -e OPENWATCH_CLEANUP=true \
  opengpunetwork/openwatch:latest
```

### docker-compose

```yaml
services:
  openwatch:
    image: opengpunetwork/openwatch:latest
    container_name: openwatch
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./openwatch.yaml:/etc/openwatch/openwatch.yaml:ro   # optional
    environment:
      - OPENWATCH_SCHEDULE=0 4 * * *
      - OPENWATCH_CLEANUP=true
      - OPENWATCH_ROLLBACK_ON_FAILURE=true
      - OPENWATCH_NOTIFY_URL=telegram://token@telegram?channels=chatid
```

OpenWatch talks to the Docker daemon through the mounted socket, polls the registry on the configured interval (or cron expression), and recreates any container whose image has moved to a new digest.

---

## Configuration

All settings can be supplied via environment variables, a YAML file, or both. Precedence is:

1. `OPENWATCH_*` environment variables (highest)
2. `./openwatch.yaml` or `/etc/openwatch/openwatch.yaml`
3. Built-in defaults (lowest)

A full annotated example lives at [`openwatch.yaml.example`](openwatch.yaml.example).

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `OPENWATCH_INTERVAL` | `86400` | Poll interval in seconds. Ignored when `OPENWATCH_SCHEDULE` is set. |
| `OPENWATCH_SCHEDULE` | _(unset)_ | Standard 5-field cron expression (local time). Overrides interval when set. |
| `OPENWATCH_CLEANUP` | `false` | Delete the previous image after a successful update. |
| `OPENWATCH_INCLUDE_STOPPED` | `false` | Also consider stopped containers. |
| `OPENWATCH_LABEL_ENABLE` | `false` | Require `openwatch.enable=true` label to opt a container in. |
| `OPENWATCH_ROLLBACK_ON_FAILURE` | `false` | Roll back to the previous image if the post-update healthcheck fails. |
| `OPENWATCH_HEALTHCHECK_TIMEOUT` | `30` | Seconds to wait for a container to report healthy after recreate. |
| `OPENWATCH_STOP_TIMEOUT` | `10` | Seconds to wait for a graceful SIGTERM shutdown before escalating to SIGKILL. |
| `OPENWATCH_NOTIFY_URL` | _(unset)_ | shoutrrr notification URL. Empty disables notifications. |
| `OPENWATCH_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error`. |
| `OPENWATCH_LOG_FORMAT` | `text` | Log format: `text` (human-readable) or `json` (structured). |
| `OPENWATCH_HTTP_API` | `false` | Enable the HTTP API on port 8080. |
| `DOCKER_HOST` | _(from env)_ | Standard Docker env var. Use `unix:///var/run/docker.sock` or `tcp://host:2376`. |
| `DOCKER_CONFIG` | _(unset)_ | Standard Docker env var. Path to the directory containing `config.json` for registry auth. |

---

## Container labels

Labels are set on the containers you want OpenWatch to manage. They override global behaviour for a single container.

| Label | Values | Description |
|---|---|---|
| `openwatch.enable` | `true` / `false` | Force include or exclude this container. `true` wins even when `OPENWATCH_LABEL_ENABLE=true` requires opt-in. |
| `openwatch.rollback` | `true` / `false` | Override `OPENWATCH_ROLLBACK_ON_FAILURE` for this container. |
| `openwatch.cleanup` | `true` / `false` | Override `OPENWATCH_CLEANUP` for this container. |
| `openwatch.notify_only` | `true` | Detect updates and send a notification, but do not pull or recreate. |
| `openwatch.stop_timeout` | integer (seconds) | Override `OPENWATCH_STOP_TIMEOUT` for this container. |
| `openwatch.depends_on` | container name | Update this container only after the named container has been updated. |

Example:

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
      openwatch.notify_only: "true"   # just tell me, don't update
```

---

## Registry authentication

OpenWatch resolves credentials per registry host. The resolution path depends on the host:

### Docker Hub, GHCR, GCR, generic private registries

These use a standard Docker `config.json`. Mount your existing login file into the container:

```bash
docker run -d \
  --name openwatch \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v $HOME/.docker/config.json:/config/config.json:ro \
  -e DOCKER_CONFIG=/config \
  opengpunetwork/openwatch:latest
```

Any host you've already authenticated to with `docker login` will Just Work. The Docker Hub aliases `docker.io`, `index.docker.io`, and `registry-1.docker.io` all resolve to the same credential entry.

### Amazon ECR

Host pattern `<account>.dkr.ecr.<region>.amazonaws.com` is detected automatically. OpenWatch calls `ecr:GetAuthorizationToken` through the AWS SDK, which reads credentials from the standard AWS environment variables (or the shared config file / IMDS, in that order):

```bash
docker run -d \
  --name openwatch \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e AWS_ACCESS_KEY_ID=AKIAEXAMPLE \
  -e AWS_SECRET_ACCESS_KEY=... \
  -e AWS_REGION=us-east-1 \
  opengpunetwork/openwatch:latest
```

If your IAM role is attached via EC2 instance metadata or IRSA, you don't need to set the key variables — the SDK picks them up automatically.

### Anonymous

Public images (official Docker Hub repos, public GHCR packages, etc.) need no configuration at all. OpenWatch falls through to anonymous access when no credentials match a host.

---

## Notifications

OpenWatch uses [shoutrrr](https://github.com/containrrr/shoutrrr) for notifications. Set `OPENWATCH_NOTIFY_URL` to any shoutrrr-compatible URL. Empty disables notifications entirely.

### Telegram

```bash
OPENWATCH_NOTIFY_URL="telegram://<bot-token>@telegram?channels=<chat-id>"
```

### Slack

```bash
OPENWATCH_NOTIFY_URL="slack://<token-a>/<token-b>/<token-c>@<channel-name>"
```

### ntfy

```bash
OPENWATCH_NOTIFY_URL="ntfy://ntfy.sh/your-topic"
```

See the [shoutrrr documentation](https://containrrr.dev/shoutrrr/) for the full list of supported providers (Discord, Gotify, Matrix, Mattermost, Microsoft Teams, Pushover, Rocket.Chat, and more).

### Events

OpenWatch emits one notification per lifecycle event:

| Event | When |
|---|---|
| `UPDATE_STARTED` | Digest mismatch detected, about to pull |
| `UPDATE_SUCCESS` | Container recreated and healthy |
| `UPDATE_FAILED` | Pull or recreate step failed |
| `UPDATE_AVAILABLE` | `notify_only` container has a new image available |
| `ROLLBACK_TRIGGERED` | Post-update healthcheck failed, rolling back |
| `ROLLBACK_SUCCESS` | Rollback completed and container is healthy again |
| `ROLLBACK_FAILED` | Rollback itself failed (critical — manual intervention needed) |

Notifications are non-blocking with a 10-second per-call timeout, so a slow or dead notification provider cannot stall the update loop.

---

## Rollback

When `OPENWATCH_ROLLBACK_ON_FAILURE=true` (or `openwatch.rollback=true` on a specific container), OpenWatch monitors the container's healthcheck after recreate and reverts to the previous image if the check fails.

The flow:

1. Before pulling the new image, OpenWatch captures the **image ID** the container was running on.
2. Pull → stop → remove → recreate → start the container with the new image.
3. Poll the container's `State.Health` every 2 seconds for up to `OPENWATCH_HEALTHCHECK_TIMEOUT` seconds.
4. If the container reports `healthy`, the update is complete and the `UPDATE_SUCCESS` notification fires.
5. If the container reports `unhealthy`, exits unexpectedly, or the timeout elapses, OpenWatch stops the new container, recreates it using the captured previous image ID with the same full config (env, volumes, ports, networks, labels, restart policy), and starts it.
6. On successful rollback, the previous image is **never cleaned up** — the container is running on it again, and deleting it would brick the rollback.

Containers without a `HEALTHCHECK` directive bypass the check entirely and are treated as successful the moment they start. A healthcheck failure with rollback disabled leaves the unhealthy container in place so an operator can inspect it.

---

## HTTP API

Enable with `OPENWATCH_HTTP_API=true`. The API listens on port `8080` inside the container — the port is fixed, so if you need it on a different host port just remap it with `-p 9000:8080` (or the docker-compose equivalent). The following endpoints are exposed:

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Liveness probe, returns the daemon version. |
| `GET` | `/metrics` | Prometheus metrics in standard exposition format. |
| `GET` | `/api/v1/containers` | JSON array of currently monitored containers. |
| `POST` | `/api/v1/update` | Trigger an immediate update check for all containers. Returns 202 and runs asynchronously. |
| `POST` | `/api/v1/update/:name` | Trigger an update for a single container by name. Returns 404 if unknown, 202 if accepted. |

### Example responses

`GET /health`:

```json
{"status":"ok","version":"latest"}
```

`GET /api/v1/containers`:

```json
[
  {
    "name": "myapp",
    "image": "myrepo/myapp:latest",
    "status": "up_to_date",
    "last_checked": "2026-04-10T12:34:56Z",
    "last_updated": "2026-04-09T22:11:00Z"
  },
  {
    "name": "nginx",
    "image": "nginx:stable",
    "status": "updating",
    "last_checked": "2026-04-10T12:34:56Z",
    "last_updated": null
  }
]
```

`POST /api/v1/update`:

```json
{"accepted":true}
```

`POST /api/v1/update/myapp`:

```json
{"accepted":true,"container":"myapp"}
```

### Metrics

The `/metrics` endpoint exposes four Prometheus metrics:

- `openwatch_updates_total{container,status}` — counter, status is `success`, `failed`, `skipped`, or `notify_only`
- `openwatch_rollbacks_total{container,status}` — counter, status is `success` or `failed`
- `openwatch_containers_monitored` — gauge, number of containers inspected on the most recent tick
- `openwatch_update_duration_seconds{container}` — histogram, wall-clock duration of a successful update from pull start to container start

---

## Migrating from Watchtower

OpenWatch is designed as a drop-in replacement. The environment variables and labels are intentionally close to Watchtower's so most deployments only need a find-and-replace.

### Environment variables

| Watchtower | OpenWatch | Notes |
|---|---|---|
| `WATCHTOWER_POLL_INTERVAL` | `OPENWATCH_INTERVAL` | Same semantics (seconds). |
| `WATCHTOWER_SCHEDULE` | `OPENWATCH_SCHEDULE` | Watchtower uses a 6-field format (with seconds); OpenWatch uses the standard 5-field cron. Drop the leading seconds column. |
| `WATCHTOWER_CLEANUP` | `OPENWATCH_CLEANUP` | Same. |
| `WATCHTOWER_INCLUDE_STOPPED` | `OPENWATCH_INCLUDE_STOPPED` | Same. |
| `WATCHTOWER_LABEL_ENABLE` | `OPENWATCH_LABEL_ENABLE` | Same. |
| `WATCHTOWER_NOTIFICATION_URL` | `OPENWATCH_NOTIFY_URL` | Same shoutrrr URL format. |
| `WATCHTOWER_DEBUG` | `OPENWATCH_LOG_LEVEL=debug` | OpenWatch uses a leveled logger. |
| `WATCHTOWER_MONITOR_ONLY` | `openwatch.notify_only=true` | Container label in OpenWatch rather than a global env var. |
| `WATCHTOWER_HTTP_API_METRICS` | `OPENWATCH_HTTP_API=true` | Enables both metrics and the REST API. |
| `WATCHTOWER_TIMEOUT` | `OPENWATCH_STOP_TIMEOUT` | Graceful shutdown timeout. |
| `DOCKER_HOST` | `DOCKER_HOST` | Unchanged — standard Docker env var. |

### Labels

| Watchtower | OpenWatch |
|---|---|
| `com.centurylinklabs.watchtower.enable` | `openwatch.enable` |
| `com.centurylinklabs.watchtower.monitor-only` | `openwatch.notify_only` |
| `com.centurylinklabs.watchtower.depends-on` | `openwatch.depends_on` |

The Docker socket mount is identical. There is no database, state file, or other persistent data to migrate — OpenWatch is stateless by design.

---

## Building from source

### Native binary

```bash
git clone https://github.com/OpenGPU-Network/openwatch.git
cd openwatch
make build
```

Or without the Makefile:

```bash
go build -ldflags="-s -w -X main.Version=$(git describe --tags --always --dirty)" -o openwatch ./cmd/openwatch
```

### Docker image

The Dockerfile is multi-arch. Use `docker buildx` to produce linux/amd64 and linux/arm64 in one command:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=$(git describe --tags --always --dirty) \
  -t openwatch:local \
  .
```

`make docker-build` wraps the same command; `make publish` adds `--push` and uses whichever Docker Hub login is already active on the host (Docker Desktop, or a prior `docker login`).

### Running tests

```bash
make test    # go test -race ./...
make vet     # go vet ./...
make lint    # golangci-lint run (install separately)
```

---

## Contributing

Issues and pull requests are welcome. A few things to keep in mind:

- Run `make test vet` before submitting a PR.
- Keep commits focused — small, reviewable changes are easier to land than big rewrites.
- New features usually need a test. Bug fixes usually need a regression test.
- For non-trivial changes, open an issue first so we can align on the approach before you invest time.

---

## License

OpenWatch is released under the MIT License. See [LICENSE](LICENSE) for the full text.
