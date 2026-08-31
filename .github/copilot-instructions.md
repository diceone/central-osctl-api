# Copilot Instructions: Central OSCTL API

## Project Overview

This is a **centralized orchestrator API** that manages multiple `osctl` API clients. It acts as a registry and proxy layer, allowing clients to register themselves and route requests through this central service to registered backend APIs.

**Core architecture**: Single-file Go HTTP server with in-memory state management - all logic lives in [main.go](../main.go).

## Quick Start

```bash
# 1. Build and start the server (with optional config)
export API_KEY="your-secret-key"  # Optional: Enable authentication (or use API_KEYS=k1,k2:ro)
export PERSISTENCE_FILE="clients.json"  # Optional: Change persistence file
go build -o central-osctl-api && ./central-osctl-api

# 2. Register a client (in another terminal)
curl -X POST http://localhost:12001/register \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{"id":"client1","api_url":"http://localhost:8080","username":"admin","password":"secret"}'

# 3. List registered clients (requires X-API-Key when auth is configured; passwords are never returned)
curl -H "X-API-Key: your-secret-key" http://localhost:12001/clients

# 4. Proxy a request to the registered client (query style or REST style)
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/proxy?client_id=client1&path=/status"
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/proxy/client1/status"

# 5. Observability endpoints
curl http://localhost:12001/healthz
curl http://localhost:12001/version
curl -H "X-API-Key: your-secret-key" http://localhost:12001/metrics   # Prometheus format
curl -H "X-API-Key: your-secret-key" http://localhost:12001/audit
```

## Key Components

- **CentralAPI struct**: Holds registered clients in a `map[string]OsctlClient` with mutex-protected concurrent access, plus per-client circuit-breaker state, health-failure counters, a fixed-window rate limiter, and an audit ring buffer
- **OsctlClient struct**: Downstream API with `ID`, `ApiURL`, `BackupURLs` (failover), `Username`, `Password` (Basic Auth), `Tags`, `SkipVerify`, `TtlSeconds`/`ExpiresAt`, `Healthy`
- **HTTP endpoints** (each enforces its HTTP method and, when keys are configured, the `X-API-Key` header):
  - `POST /register` - Full register (JSON body); `PATCH /register` - partial update (nil fields preserved, `ttl_seconds: 0` clears expiry)
  - `POST /unregister` - Remove client (JSON body with `id` field); unknown IDs return 404
  - `GET /clients` - List all registered clients (JSON map; `password` always empty; `?tag=a&tag=b` = AND tag filter; expired clients are filtered out)
  - `GET /proxy?client_id=X&path=/endpoint` - Legacy query-style proxy
  - `GET|POST|... /proxy/{clientID}/...` - REST-style proxy (everything after `/proxy/{id}` is the upstream path)
  - `GET /healthz` - Liveness (no auth)
  - `GET /version` - Build metadata (no auth)
  - `GET /metrics` - Prometheus text format
  - `GET /audit` - Audit ring buffer (requires full-permission key)

**Example /clients response** (passwords are never exposed):
```json
{
  "client1": {
    "id": "client1",
    "api_url": "http://localhost:8080",
    "username": "admin",
    "password": "",
    "tags": ["prod"],
    "healthy": true
  }
}
```

## Development Workflow

### Building and Running Locally

```bash
go build -o central-osctl-api
./central-osctl-api  # Starts on port 12001 (configurable via PORT env)
```

Port defaults to `12001` but can be configured via `PORT` environment variable - see the end of [main.go](../main.go). Graceful shutdown on SIGINT/SIGTERM (10s drain). TLS is enabled by setting both `HTTPS_CERT` and `HTTPS_KEY`.

### Testing

```bash
go test ./...
go test -race ./...
```

Integration-style tests live in `main_test.go` (built with `httptest`), covering registration validation, PATCH updates, authentication (single + multi-key with read-only `:ro` keys), rate limiting (429 + Retry-After), TTL expiry, tag filtering, proxy forwarding/failover/body replay, circuit breaker (503 when open), health sweeps + auto-deregister, health-check-on-register, TLS skip_verify, live file reload, metrics/audit/version endpoints, request-ID middleware, and REST-style proxying through the real mux.

### Docker Deployment

Multi-stage build using `golang:1.26-alpine` (pinned by digest) and a pinned Alpine runtime. The container runs as a non-root `app` user with `/data` as the working directory, so the default persistence file lives in `/data/clients.json`. A `HEALTHCHECK` probes `/healthz` (set `HEALTHCHECK_PORT` when changing `PORT`):
```bash
docker build -t central-osctl-api .

# Run with default settings
docker run -p 12001:12001 central-osctl-api

# Run with custom configuration
docker run -p 8080:8080 \
  -e PORT=8080 -e HEALTHCHECK_PORT=8080 \
  -e API_KEY=secret-key \
  -e PERSISTENCE_FILE=/data/clients.json \
  -v $(pwd)/data:/data \
  central-osctl-api
```

### Systemd Service

Production deployment uses systemd:

```bash
sudo cp systemd/central-osctl-api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now central-osctl-api
sudo systemctl status central-osctl-api
```

Service runs with `GOMAXPROCS=4`, a dynamic non-root user, `StateDirectory=central-osctl-api` (`PERSISTENCE_FILE=/var/lib/central-osctl-api/clients.json`), and auto-restarts on failure. Set keys via `systemctl edit` (`Environment=API_KEYS=...`).

## Code Patterns

### Concurrency Safety

All client map access is protected with `api.mu.Lock()` / `defer api.mu.Unlock()`. Proxy path resolution locks to read the client, then **unlocks before HTTP calls** to avoid blocking concurrent requests. The metrics registry, rate limiter, breakers, audit ring, and failure counters all share `api.mu` (short critical sections, never held across I/O).

### HTTP Request Proxying

- Two styles: legacy `/proxy?client_id=X&path=/ram` and REST `/proxy/{id}/ram` (both resolved by `resolveProxy`; `..` segments rejected; unknown/expired clients 404)
- `proxyDispatch` tries `api_url` then `backup_urls` in order; bodies up to 1 MiB are buffered and replayed for failover, larger/unknown bodies stream to the primary only
- Per-attempt `context.WithTimeout` (upstream timeout); the attempt context outlives the response body copy (cancel deferred to handler return)
- Circuit breaker: consecutive failures open the breaker for a cooldown (503 short-circuit), then a half-open probe
- WebSocket/upgrade requests (`Upgrade` header + `Connection: Upgrade`) go through `httputil.ReverseProxy` with `FlushInterval: -1`
- Basic Auth from the registered client is set last, overriding caller-supplied `Authorization`; hop-by-hop headers are stripped in both directions
- Query parameters registered in the client's `api_url` act as defaults and can be overridden by the proxied request

### Logging & Observability

- **All logs are JSON lines** via `logEvent(event, fields)` - filter on the `event` field (`startup`, `clients_loaded`, `http_request`, `proxy_upstream_failed`, ...)
- The `/metrics` endpoint renders the Prometheus text format from an in-memory registry (no client library)
- The `/audit` endpoint returns the in-memory audit ring (admin actions + proxied requests, 100 entries)
- Every response carries an `X-Request-ID` header (generated UUID-style or echoed, sanitized to a safe charset)

### Error Handling

Standard `http.Error()` responses with appropriate status codes.

**HTTP Status Codes**:
- `200 OK` - Successful register/unregister, successful proxy
- `400 Bad Request` - Missing/invalid client_id, path (`..` rejected), or JSON body; invalid URL; unreachable client when `HEALTH_CHECK_ON_REGISTER` is set
- `401 Unauthorized` - Missing or incorrect X-API-Key
- `403 Forbidden` - Read-only (`:ro`) key used on a write endpoint
- `404 Not Found` - Client not found/expired
- `405 Method Not Allowed` (with `Allow` header)
- `413 Request Entity Too Large` - Management body over 1 MiB
- `429 Too Many Requests` - Rate limit hit (with `Retry-After`)
- `500 Internal Server Error` - Persistence write failures, JSON encoding errors
- `502 Bad Gateway` - All upstream candidates failed
- `503 Service Unavailable` - Circuit breaker open

## Configuration

**Environment Variables**:
- `PORT` - Server port (default: `12001`)
- `PERSISTENCE_FILE` - JSON file for client persistence (default: `clients.json`)
- `API_KEY` - Legacy single API key (full permissions)
- `API_KEYS` - Comma-separated keys; `key:ro` = read-only (list + proxy only)
- `RATE_LIMIT_PER_MINUTE` - Fixed-window rate limit per key/IP (0 = off)
- `HEALTH_CHECK_PATH` (default `/status`), `HEALTH_CHECK_INTERVAL` (0 = off), `HEALTH_CHECK_ON_REGISTER`, `HEALTH_CHECK_THRESHOLD` (default 3), `AUTO_DEREGISTER`
- `FILE_RELOAD_INTERVAL` - External persistence-file reload check (0 = off)
- `HTTPS_CERT` / `HTTPS_KEY` - Serve TLS when both are set

**State Management**: 
- Clients are persisted atomically (temp file + rename, `0600`); loaded at startup; saved after each successful register/unregister with map rollback on failure (500)
- With `FILE_RELOAD_INTERVAL`, external edits trigger reload (`savedContent` comparison avoids reloading the service's own writes)
- TTL sweep (every 30s) removes expired clients; health sweep (when interval set) updates `healthy`/failure counters and can auto-deregister
- Background jobs run in `runBackground(ctx)` and stop with the shutdown signal

**Security Features**:
- Multi-key auth with per-key read-only permissions, constant-time comparison, rate limiting
- URL validation, `..` rejection, hop-by-hop stripping, 1 MiB management body cap
- Passwords redacted in `/clients`; upstream credentials set last so caller headers cannot override

## Critical Gotchas

⚠️ **File Permissions**: Persistence file is created with `0600` permissions. Ensure the process has write access to the directory.

⚠️ **Client Update**: `POST /register` with the same ID overwrites **all** fields; `PATCH /register` merges only provided fields.

⚠️ **Password Security**: Passwords live in plain text in the JSON file and are redacted in `/clients`. Rotate by re-registering/PATCHing the client.

⚠️ **Streaming bodies**: Request bodies with unknown length or > 1 MiB cannot be replayed - they stream to the primary upstream only (no failover).

⚠️ **WriteTimeout is 0** (intentional, for long-lived proxied/streaming responses). Do not "fix" this by adding a finite WriteTimeout without considering WebSocket/streaming use.

⚠️ **Auth in handlers, middleware adds metadata**: method checks + auth live inside each handler (so direct handler calls in tests enforce them); the `wrap()` middleware adds request IDs, metrics, and access logs.

## Troubleshooting

Logs are JSON; grep for the `event` field.

**Problem**: Registration returns `401 Unauthorized`
- **Solution**: Check `X-API-Key` matches `API_KEY` or an entry in `API_KEYS`. Startup logs `auth_disabled` when no key is configured.

**Problem**: `403 Forbidden`
- **Solution**: A `key:ro` read-only key was used on a write endpoint (register/unregister). Use a full-permission key.

**Problem**: `429 Too Many Requests`
- **Solution**: `RATE_LIMIT_PER_MINUTE` was hit; the response includes `Retry-After`. Disable by setting the variable to `0`.

**Problem**: Persistence file not saving (500 responses)
- **Solution**: Check directory write permissions. With systemd, `StateDirectory` handles this. Server-side failures are logged as JSON events.

**Problem**: `400 Bad Request: invalid api_url`
- **Solution**: Ensure URL includes scheme (`http://` / `https://`). Invalid: `localhost:8080`.

**Problem**: `502 Bad Gateway` on proxy
- **Solution**: All upstream candidates unreachable. Check `api_url`/`backup_urls`, credentials, and TLS trust (`skip_verify: true` for self-signed downstreams). Details in JSON logs (`proxy_upstream_failed`).

**Problem**: `503 Service Unavailable` on proxy
- **Solution**: Circuit breaker is open after consecutive failures; it half-opens automatically after the cooldown and resets on the next successful request.

**Problem**: Clients lost after restart
- **Solution**: Check `PERSISTENCE_FILE`. Startup logs `clients_loaded` with a count.

**Problem**: Expired clients (TTL)
- **Solution**: Clients with `ttl_seconds` are removed by the expiry sweep (log event `clients_expired`); re-register or PATCH with `ttl_seconds: 0` to keep them.

## Project Conventions

- **No dependencies**: Standard library only (`net/http`, `encoding/json`, `crypto/subtle`, `os`, `sync`, `time`, ...)
- **Go version**: 1.25 in [go.mod](../go.mod) (Go 1.22+ mux method patterns)
- **State management**: JSON file persistence (atomic writes)
- **Logging**: Structured single-line JSON events via `logEvent` (no logging framework)
- **Configuration**: Environment variables only
- **Security**: Optional API key auth (`API_KEY`/`API_KEYS`), rate limiting, optional direct TLS
- **Formatting/lint**: Use `gofmt` and `go vet`; tests via `go test -race ./...`

## Integration Points

This service coordinates with:
- **Downstream osctl APIs**: HTTP APIs that register themselves (optionally with `backup_urls`, `tags`, `ttl_seconds`)
- **Upstream consumers**: Services/users that query `/clients`, call `/proxy`, and scrape `/metrics`

No message queues, databases, or external service dependencies.
