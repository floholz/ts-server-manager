# ts-server-manager

A small Go sidecar that holds a long-lived [TeamSpeak 3 ServerQuery](https://yat.qa/ressourcen/server-query-kommandos/) (SSH) connection to a TeamSpeak server and exposes a minimal HTTP API for health checks, server info, and Docker Hub update detection. Designed to run alongside a TeamSpeak server container in the same compose stack.

## Endpoints

| Method | Path                  | Purpose                                                        |
|--------|-----------------------|----------------------------------------------------------------|
| GET    | `/healthz`            | Liveness — 200 whenever the process is running                 |
| GET    | `/readyz`             | Readiness — 200 only when the TS connection is live, else 503  |
| GET    | `/api/server-info`    | Selected vserver metadata via ServerQuery                      |
| GET    | `/api/update-check`   | Compare running TS version against Docker Hub `latest` digest  |

`/api/update-check` accepts `?refresh=1` to bypass the cache.

### Example responses

`GET /api/server-info`
```json
{
  "version":         "6.0.0-beta9",
  "platform":        "Linux",
  "build":           1700000000,
  "sid":             1,
  "server_status":   "online",
  "server_name":     "Example TS",
  "uptime_seconds":  123456,
  "clients_online":  3,
  "clients_max":     32,
  "login_name":      "serveradmin"
}
```

`GET /api/update-check`
```json
{
  "version_running":  "6.0.0-beta9",
  "version_latest":   "6.0.0-beta10",
  "update_available": true,
  "checked_at":       "2026-05-02T12:34:56Z"
}
```

When the running version's tag isn't published on Docker Hub (e.g. custom builds), the response includes a `note` field and `update_available` is `false`.

## Quick start

```bash
cp .env.example .env
# edit .env with your ServerQuery credentials
docker compose up -d
curl http://localhost:9988/readyz
```

## Configuration

All configuration is via environment variables. See [`.env.example`](.env.example).

| Variable             | Default                                | Required | Notes                                  |
|----------------------|----------------------------------------|----------|----------------------------------------|
| `TS_HOST`            | —                                      | yes      | TeamSpeak server hostname              |
| `TS_PORT`            | `10022`                                | no       | ServerQuery SSH port                   |
| `TS_USER`            | —                                      | yes      | ServerQuery user                       |
| `TS_PASSWORD`        | —                                      | yes      | ServerQuery password                   |
| `TS_SID`             | `1`                                    | no       | Virtual server ID                      |
| `HTTP_ADDR`          | `:9988`                                | no       | Bind address for the HTTP server       |
| `LOG_LEVEL`          | `info`                                 | no       | `debug`, `info`, `warn`, `error`       |
| `DOCKERHUB_REPO`     | `teamspeaksystems/teamspeak6-server`   | no       | Image repo to query for updates        |
| `UPDATE_CHECK_TTL`   | `15m`                                  | no       | Cache freshness window (Go duration)   |

## Local development with TeamSpeak

The repo ships a second compose file that brings up a real TeamSpeak 6 server alongside the manager for local testing:

```bash
docker compose -f compose.teamspeak.yaml up -d         # start TS + MariaDB
docker compose up -d                                   # start the manager
docker logs ts_server | grep token                     # grab the admin token
# set TS_PASSWORD in .env to the ServerQuery password from the TS logs
docker compose restart ts-server-manager
```

The TeamSpeak compose file is for development only — do not deploy as-is in production (root DB password, no TLS, etc.).

### Building locally

```bash
go build -o ts-server-manager .
TS_HOST=localhost TS_USER=serveradmin TS_PASSWORD=... ./ts-server-manager
```

### Running the test suite

```bash
go test -race ./...
```

### Container build

```bash
docker build -t ts-server-manager:dev \
  --build-arg VERSION=v0.0.0-dev \
  --build-arg REVISION=$(git rev-parse HEAD) .
```

The CI workflow at [`.github/workflows/release.yml`](.github/workflows/release.yml) builds and publishes images to `ghcr.io/floholz/ts-server-manager` on every `v*` tag push.

## Architecture

Four loosely-coupled internal packages:

| Package              | Responsibility                                                                |
|----------------------|-------------------------------------------------------------------------------|
| `internal/config`    | Environment-driven configuration with validation                              |
| `internal/ts`        | Long-lived TS3 ServerQuery client — reconnect, exponential backoff, keepalive |
| `internal/dockerhub` | Docker Hub digest comparison with a 15-minute in-memory cache                 |
| `internal/api`       | HTTP handlers, consumer-side interfaces, request-id + access-log middleware   |

`main.go` wires the units, runs `http.Server`, and handles SIGINT/SIGTERM with a 10-second graceful shutdown deadline. Logs are JSON via `log/slog`, OTel-shaped for future Loki / collector integration.

## Versioning

Released versions are pushed to GitHub Container Registry on each `v*` git tag:

- `ghcr.io/floholz/ts-server-manager:vX.Y.Z` — exact tag
- `ghcr.io/floholz/ts-server-manager:X.Y.Z`  — semver-stripped
- `ghcr.io/floholz/ts-server-manager:latest` — last published version

## License

[GPL-3.0-only](LICENSE).
