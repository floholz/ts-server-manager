# v1 API and Update Check — Design

**Date:** 2026-05-02
**Status:** Approved, ready for implementation planning
**Scope:** v1 of `ts-server-manager`. A small sidecar service that exposes a minimal HTTP API around a long-lived TeamSpeak 3 ServerQuery (SSH) connection, plus an update check against Docker Hub.

## Goals

- Expose three application endpoints — server info, update check — plus standard liveness and readiness probes.
- Hold a single long-lived ServerQuery connection that survives TS restarts and idle timeouts.
- Detect whether the running TeamSpeak server image is the latest available on Docker Hub.
- Stay tiny and self-contained: stdlib HTTP, structured JSON logs, no auth, no persistence. Suitable as a compose sidecar.

## Non-goals (deferred to a later version)

- PocketBase integration (HTTP server, auth, admin UI, SQLite persistence).
- OpenTelemetry SDK / OTLP export. v1 emits structured JSON logs that are OTel-shaped and Loki-ingestible; the SDK swap happens later.
- Saving and restoring TeamSpeak configuration (channels, roles, permissions).
- Any UI.
- API authentication. v1 is intended to run on an internal compose network, not exposed publicly.
- Integration tests against a real TS server.

## Endpoints

| Method | Path                 | Purpose                                                |
|--------|----------------------|--------------------------------------------------------|
| GET    | `/healthz`           | Liveness — 200 whenever the process is running         |
| GET    | `/readyz`            | Readiness — 200 only when the TS connection is live    |
| GET    | `/api/server-info`   | TS server metadata via ServerQuery                     |
| GET    | `/api/update-check`  | Compare running TS version against Docker Hub `latest` |

Health and readiness sit at the root, application endpoints under `/api/`. This separates infrastructure probes from application surface and is consistent with Kubernetes conventions.

### `GET /healthz`

Always returns `200 OK` with `{"status":"ok"}` while the process is alive. Does not call into TS.

### `GET /readyz`

- `200 OK` `{"status":"ready"}` when the TS3 client reports `IsConnected() == true`.
- `503 Service Unavailable` `{"status":"not_ready","ts":"down"}` otherwise.

### `GET /api/server-info`

Returns TS server metadata. On success, response body:

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

Source data: `client.Version()`, `client.Whoami()`, plus one `serverinfo` ServerQuery call.

Errors:
- `503` if TS not connected, or if a transport error occurs during the ServerQuery call. The transport error also triggers a reconnect in the background loop.

### `GET /api/update-check`

Query params:
- `refresh=1` — bypass the cache and fetch fresh data from Docker Hub.

Response body on success:

```json
{
  "version_running":   "6.0.0-beta9",
  "version_latest":    "6.0.0-beta10",
  "update_available":  true,
  "checked_at":        "2026-05-02T12:34:56Z"
}
```

Optional fields:
- `note` — present only when the running version's tag is not found on Docker Hub (e.g. custom builds). Value: `"running version not found on docker hub"`. In this case `version_latest` is `""` and `update_available` is `false`.

Errors:
- `503` if Docker Hub is unreachable (DNS / connection error).
- `502` if Docker Hub returns a 5xx.
- `400` if `refresh` has an invalid value.

## Update-check algorithm

Repository: `teamspeaksystems/teamspeak6-server` (configurable via env). Cache TTL: 15 minutes (configurable).

On request to `/api/update-check`:

1. Read the running version: `ts.Version()` (cheap, hits the long-lived connection).
2. If the cache holds a result younger than the TTL **and** `refresh=1` is not set, return it.
3. Otherwise refetch:
   1. `GET https://hub.docker.com/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest` → `digest_latest`.
   2. `GET https://hub.docker.com/v2/repositories/teamspeaksystems/teamspeak6-server/tags/{running_version}` → `digest_running`.
      - On `404`: short-circuit with `version_latest=""`, `update_available=false`, `note="running version not found on docker hub"`. Cache and return.
   3. If `digest_running == digest_latest`: `update_available=false`, `version_latest=running_version`. Cache and return.
   4. If `digest_running != digest_latest`: make one more call — `GET .../tags/?page_size=100` — find the tag whose digest equals `digest_latest` (excluding the `latest` tag itself), and use its name as `version_latest`. If no match is found on page 1, leave `version_latest=""` but still return `update_available=true`.
4. Stamp `checked_at = time.Now()`, store in cache, return.

The third call (tag list lookup) is only made when there is a mismatch. The common case (no update) is two calls.

## Architecture

A single Go binary, four loosely-coupled units. Each unit is independently testable; the HTTP layer depends on the other two via interfaces.

```
main.go
  - load config from env
  - construct units, wire them, start HTTP server
  - graceful shutdown on SIGINT/SIGTERM
       │
       ├──── internal/ts ──────────────────────────┐
       │       TS3 ServerQuery client wrapper      │
       │       - long-lived connection             │
       │       - reconnect with exponential backoff│
       │       - keepalive every ~3 minutes        │
       │       - thread-safe (mutex around client) │
       │                                           │
       ├──── internal/dockerhub ───────────────────┤
       │       Docker Hub digest client + cache    │
       │       - tags/latest                       │
       │       - tags/{version}                    │
       │       - tag list (only on mismatch)       │
       │                                           │
       └──── internal/api ─────────────────────────┘
               HTTP handlers, registers routes
               on a *http.ServeMux passed in
```

### `internal/ts`

```go
type Config struct {
    Host, Port, User, Password string
    SID                        int
}

type VersionInfo struct {
    Version, Platform string
    Build             int
}

type ServerInfo struct {
    Version, Platform string
    Build             int
    SID               int
    ServerStatus      string
    ServerName        string
    UptimeSeconds     int64
    ClientsOnline     int
    ClientsMax        int
    LoginName         string
}

type Client struct { /* unexported */ }

func New(cfg Config) *Client
func (c *Client) Start(ctx context.Context)        // launches background loop
func (c *Client) IsConnected() bool                // for /readyz
func (c *Client) Version() (VersionInfo, error)
func (c *Client) ServerInfo() (ServerInfo, error)
```

`Start` launches a goroutine that:

1. Connects: SSH dial, ServerQuery login, `use sid=N`. On failure, logs at WARN, sleeps backoff, retries.
2. On successful connect, sets `connected=true`, logs INFO, ticks a keepalive (`version` ping) every 3 minutes.
3. On a transport error (keepalive fails, or a handler call hits a network/IO error from the underlying SSH client — distinct from a ServerQuery-level error response such as "permission denied" or "invalid SID", which is returned to the caller without dropping the connection), sets `connected=false`, closes the underlying client, signals the connect loop to retry.
4. Backoff is exponential, starting at 1s, doubling, capped at 30s. Reset to 1s after a successful connection.
5. Exits cleanly when the context is cancelled.

`Version()` and `ServerInfo()` acquire the mutex, run the underlying ServerQuery commands, and on transport error return a typed `ErrUnavailable`. The API layer maps that to 503. The error also flips `connected=false` and signals the loop to reconnect.

The `multiplay/go-ts3` (replaced with the `rocketsciencegg/go-ts3` fork in `go.mod`) `*ts3.Client` is **not** safe for concurrent use, so all calls are serialized through the mutex.

### `internal/dockerhub`

```go
type Config struct {
    Repo string        // e.g. "teamspeaksystems/teamspeak6-server"
    TTL  time.Duration // cache TTL
}

type Result struct {
    VersionRunning  string    `json:"version_running"`
    VersionLatest   string    `json:"version_latest"`
    UpdateAvailable bool      `json:"update_available"`
    CheckedAt       time.Time `json:"checked_at"`
    Note            string    `json:"note,omitempty"`
}

type Client struct { /* unexported, holds *http.Client, cache, mu */ }

func New(cfg Config) *Client
func (c *Client) Check(ctx context.Context, runningVersion string, forceRefresh bool) (Result, error)
```

The cache holds a single `Result` (the running version doesn't change at runtime). It's keyed by `runningVersion` and invalidated automatically if a different version comes through (defensive — shouldn't happen in v1).

HTTP client: `&http.Client{Timeout: 10 * time.Second}`. No retries on Docker Hub errors in v1; the next request will retry naturally.

Errors:
- `ErrUpstreamUnreachable` — DNS/connection failures. Maps to 503.
- `ErrUpstreamError` — Docker Hub 5xx. Maps to 502.
- `404` on `/tags/{running_version}` is **not** an error — it produces a successful `Result` with the `note` field set.

### `internal/api`

```go
type TSClient interface {
    IsConnected() bool
    Version() (ts.VersionInfo, error)
    ServerInfo() (ts.ServerInfo, error)
}

type DockerHubClient interface {
    Check(ctx context.Context, runningVersion string, forceRefresh bool) (dockerhub.Result, error)
}

func Register(mux *http.ServeMux, tsClient TSClient, hub DockerHubClient, logger *slog.Logger)
```

Handlers are thin: parse query params, call the relevant unit, marshal JSON. A single middleware wraps every handler:

- Recovers panics and logs them at ERROR, returns 500 with `{"error":"internal error"}`.
- Generates a per-request ID (16-char hex from `crypto/rand`).
- Emits one access log line per request (see Logging).

### `main.go`

1. Load config from env. Validate required fields. Fail fast on config errors.
2. Build a root `context.Context` cancelled by SIGINT/SIGTERM.
3. Construct `ts.Client` and `dockerhub.Client`.
4. Call `tsClient.Start(ctx)`.
5. Build `*http.ServeMux`, call `api.Register(...)`.
6. Run `&http.Server{Addr: cfg.HTTPAddr, Handler: mux, BaseContext: func(_ net.Listener) context.Context { return ctx }}`.
7. On signal, shutdown the HTTP server with a 10-second deadline, then return.

A small `healthcheck` subcommand: `ts-server-manager healthcheck` issues `GET http://127.0.0.1:9988/readyz` with a 2s timeout and exits 0 on 200, 1 otherwise. Used by Docker `HEALTHCHECK` since the distroless runtime has no `wget`/`curl`.

## Configuration

| Env var               | Default                                  | Required | Notes                              |
|-----------------------|------------------------------------------|----------|------------------------------------|
| `TS_HOST`             | —                                        | yes      | TS server hostname                 |
| `TS_PORT`             | `10022`                                  | no       | ServerQuery SSH port               |
| `TS_USER`             | —                                        | yes      | ServerQuery user                   |
| `TS_PASSWORD`         | —                                        | yes      | ServerQuery password               |
| `TS_SID`              | `1`                                      | no       | Virtual server ID                  |
| `HTTP_ADDR`           | `:9988`                                  | no       | Bind address for the HTTP server   |
| `LOG_LEVEL`           | `info`                                   | no       | `debug`/`info`/`warn`/`error`      |
| `DOCKERHUB_REPO`      | `teamspeaksystems/teamspeak6-server`     | no       | Override the upstream image repo   |
| `UPDATE_CHECK_TTL`    | `15m`                                    | no       | Parsed by `time.ParseDuration`     |

`.env.example` is updated to list every variable with an inline comment.

## Logging

`log/slog` with a JSON handler on stdout. Field names chosen to line up with OpenTelemetry log conventions, so a future swap to an OTel handler is a one-line change in `main.go`.

Base attributes on every line:

- `service.name` — `ts-server-manager`
- `service.version` — set at build time via `-ldflags="-X main.version=..."`

Per-request access log line:

```json
{
  "time":         "2026-05-02T12:34:56.789Z",
  "level":        "INFO",
  "msg":          "http_request",
  "method":       "GET",
  "path":         "/api/update-check",
  "status":       200,
  "duration_ms":  42,
  "remote_addr":  "10.0.0.5:54321",
  "request_id":   "a1b2c3d4e5f6a7b8"
}
```

Level conventions:

- INFO — startup, shutdown, successful connect, every HTTP request, cache miss/hit summaries.
- WARN — reconnect attempts, transient TS/Docker Hub failures.
- ERROR — repeated failures, panics, unrecoverable startup errors.
- DEBUG — full request/response bodies, ServerQuery command traces.

`LOG_LEVEL=debug` raises verbosity. Default is INFO.

No OpenTelemetry SDK in v1. The OTel migration in a later version replaces the JSON handler with `otelslog`, adds an OTLP exporter, and wraps the HTTP handler in `otelhttp` — all in `main.go`.

## Error matrix

| Scenario                                      | Endpoint              | Status | Body                                                 |
|-----------------------------------------------|-----------------------|--------|------------------------------------------------------|
| Process up, TS not connected                  | `/healthz`            | 200    | `{"status":"ok"}`                                    |
| Process up, TS not connected                  | `/readyz`             | 503    | `{"status":"not_ready","ts":"down"}`                 |
| Process up, TS connected                      | `/readyz`             | 200    | `{"status":"ready"}`                                 |
| TS query fails (transport)                    | `/api/server-info`    | 503    | `{"error":"ts unavailable"}` + reconnect triggered    |
| TS disconnected at request time               | `/api/server-info`    | 503    | `{"error":"ts unavailable"}`                         |
| Docker Hub unreachable                        | `/api/update-check`   | 503    | `{"error":"upstream unreachable"}`                   |
| Docker Hub 404 for running version            | `/api/update-check`   | 200    | normal body, `version_latest:""`, `note:"..."`        |
| Docker Hub 5xx                                | `/api/update-check`   | 502    | `{"error":"upstream error"}`                         |
| Bad query param (e.g. `refresh=foo`)          | `/api/update-check`   | 400    | `{"error":"invalid refresh value"}`                  |
| Panic in any handler                          | any                   | 500    | recovered, `{"error":"internal error"}`              |

## Deployment

### Dockerfile

The existing multi-stage Dockerfile already uses `golang:1.22-alpine` and a distroless nonroot runtime. Changes for v1:

- Add `EXPOSE 9988`.
- Add a `HEALTHCHECK` directive that invokes the binary's new `healthcheck` subcommand:
  ```dockerfile
  HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/ts-server-manager", "healthcheck"]
  ```

No data volume is needed in v1 (no persistence).

### compose.yaml

```yaml
services:
  ts-server-manager:
    build: .
    image: ts-server-manager:latest
    container_name: ts-server-manager
    env_file: .env
    restart: unless-stopped
    ports:
      - "9988:9988"
```

The Dockerfile-defined `HEALTHCHECK` is sufficient; no need to redeclare in compose.

## Testing

| Package              | Approach                                                                                                  |
|----------------------|-----------------------------------------------------------------------------------------------------------|
| `internal/ts`        | Unit tests with a fake transport (interface around `*ts3.Client`). Cover: initial connect, reconnect after error, backoff sequence, keepalive failure triggers reconnect, concurrent calls serialized correctly. |
| `internal/dockerhub` | Unit tests with `httptest.Server` standing in for Docker Hub. Cover: cache hit, cache miss, force refresh, digest match (no extra call), digest mismatch (tag list lookup), 404 on running version, 5xx, network error. |
| `internal/api`       | Table-driven handler tests with mock `TSClient` and `DockerHubClient` interfaces. Asserts status codes, JSON shapes, error mapping, query param parsing. |
| `main.go`            | Wiring only, not unit-tested.                                                                             |

No integration test against a real TS server in v1. A `scripts/manual-test.sh` (or `Makefile` target) documents how to run against a local TS container.

## Open questions

None at spec time. Any new questions encountered during implementation should be raised in the implementation plan.
