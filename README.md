# Central OSCTL API

The `central-osctl-api` is a central orchestrator API that manages and interacts with multiple `osctl` APIs. It provides endpoints to register, deregister, and list `osctl` API clients, as well as proxy requests to the registered clients.

## Features

**Client management**

- **Register `osctl` API clients** with URL validation (`POST /register`)
- **Partial updates** via `PATCH /register` - only the fields you send change
- **Deregister clients** (`POST /unregister`)
- **List clients** (`GET /clients`) with passwords redacted and `?tag=` filtering
- **Client tags** - attach labels and filter listings by tag (AND semantics)
- **TTL/auto-expiry** - register clients with a `ttl_seconds`; expired clients are swept and removed automatically

**Proxying**

- **Proxy requests** with query parameter filtering (`GET /proxy?client_id=X&path=/ram`)
- **REST-style proxy** - `GET /proxy/{clientID}/ram?unit=gb`
- **Automatic failover** - register `backup_urls`; if the primary is unreachable, the next candidate is tried (replayable request bodies are re-sent)
- **Circuit breaker** - clients that keep failing get short-circuited with `503` and automatically re-probed after a cooldown
- **WebSocket / upgrade proxying** - `Upgrade` requests are handed to a streaming reverse proxy

**Operations & observability**

- **Structured JSON logging** - every request and noteworthy event is logged as one JSON line (with request IDs)
- **Request IDs** - every response gets an `X-Request-ID` header (generated or echoed)
- **Prometheus `/metrics`** - request counters, error counters, and request duration summaries
- **Audit trail** (`GET /audit`) - in-memory ring buffer of admin actions and proxied requests
- **Health endpoints** - `/healthz` (service liveness) and `/version` (build metadata)
- **Downstream health checks** - periodic probes mark clients healthy/unhealthy, optionally deregister persistent failures (`AUTO_DEREGISTER`), and can reject unreachable clients at registration (`HEALTH_CHECK_ON_REGISTER`)

**Security**

- **API key authentication** - a single legacy key (`API_KEY`) or multiple keys with per-key read-only permissions (`API_KEYS=key,key2:ro`)
- **Rate limiting** - fixed-window per key/IP (`RATE_LIMIT_PER_MINUTE`) with `429 + Retry-After`
- **TLS** - serve HTTPS directly via `HTTPS_CERT`/`HTTPS_KEY`
- **Crash-safe persistence** - atomic JSON file writes (write temp file + rename, `0600` permissions)
- **Live config reload** - external changes to the persistence file are picked up automatically when `FILE_RELOAD_INTERVAL` is set
- **TLS upstream verification** - per-client `skip_verify` flag for self-signed downstreams

## Installation

### Prerequisites

- Go 1.22 or later (the mux uses the Go 1.22+ method-pattern routing)

### Building from Source

1. **Clone the Repository**

   ```sh
   git clone https://github.com/diceone/central-osctl-api.git
   cd central-osctl-api
   ```

2. **Build the Binary**

   ```sh
   go build -o central-osctl-api
   ```

### Running the Central API

1. **Configure Environment Variables (Optional)**

   ```sh
   export PORT=12001                      # Server port (default: 12001)
   export PERSISTENCE_FILE=clients.json   # Client storage file (default: clients.json)
   export API_KEY=your-secret-key         # Enable authentication (optional)
   ```

2. **Run the Central API**

   ```sh
   ./central-osctl-api
   ```

3. **Verify the Server is Running**

   ```sh
   curl http://localhost:12001/healthz   # liveness (no auth)
   curl -H "X-API-Key: your-secret-key" http://localhost:12001/clients
   ```

The server shuts down gracefully on `SIGINT`/`SIGTERM` (in-flight requests get up to 10s to finish).

## Usage

### Register a Client

```sh
curl -X POST \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{
        "id": "client1",
        "api_url": "http://localhost:12000",
        "username": "admin",
        "password": "password",
        "backup_urls": ["http://localhost:12010"],
        "tags": ["prod", "eu"],
        "skip_verify": false,
        "ttl_seconds": 0
      }' \
  http://localhost:12001/register
```

**Client fields**

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique client identifier (required) |
| `api_url` | string | Primary upstream base URL (required; valid http/https with host) |
| `backup_urls` | []string | Optional failover candidates, tried in order |
| `username` / `password` | string | Basic Auth credentials sent to upstreams |
| `tags` | []string | Free-form labels; filter listings with `?tag=` |
| `skip_verify` | bool | Skip TLS verification for this upstream (self-signed certs) |
| `ttl_seconds` | int | Optionally auto-expire the client after N seconds |

**Note**: The `api_url` must be a valid HTTP or HTTPS URL. Invalid URLs will be rejected at registration time.

### Update a Client (PATCH)

`PATCH /register` performs a partial update - only provided fields change, everything else is preserved. Registering with the same `id` via `POST` still performs a full overwrite. `ttl_seconds: 0` removes an expiry.

```sh
curl -X PATCH \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{"id": "client1", "password": "newpass"}' \
  http://localhost:12001/register
```

### Deregister a Client

```sh
curl -X POST \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{"id": "client1"}' \
  http://localhost:12001/unregister
```

### List Clients

```sh
curl -H "X-API-Key: your-secret-key" http://localhost:12001/clients
```

Filter by tags (a client must carry **all** requested tags):

```sh
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/clients?tag=prod&tag=eu"
```

Passwords are always redacted.

### Proxy a Request

**Query style** - additional query parameters (except `client_id` and `path`) are forwarded; parameters already present in the registered `api_url` act as defaults:

```sh
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/proxy?client_id=client1&path=/ram&sort=asc"
# Forwards to: http://localhost:12000/ram?sort=asc
```

**REST style** - everything after `/proxy/{clientID}` becomes the upstream path:

```sh
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/proxy/client1/ram?unit=gb"
# Forwards to: http://localhost:12000/ram?unit=gb
```

With failover configured, an unreachable primary transparently fails over to `backup_urls` (request bodies up to 1 MiB are buffered and replayed; larger bodies stream to the primary only).

## Observability

| Endpoint | Auth | Description |
|----------|------|-------------|
| `/healthz` | none | Liveness (`status: ok`) |
| `/version` | none | Build metadata (`version`, `commit`, `commit_date`, `tree_state`) |
| `/metrics` | key | Prometheus text format (counters + duration summaries) |
| `/audit` | key (write) | JSON array of recent admin/proxy audit events (ring buffer, 100 entries) |

**JSON logs** - every event is a single JSON line, e.g.:

```json
{"time":"2026-01-01T12:00:00Z","event":"http_request","method":"GET","path":"/clients","status":200,"duration_ms":1,"request_id":"..."}
{"time":"...","event":"clients_loaded","count":2}
{"time":"...","event":"startup","version":"...","port":"12001","tls":false}
```

Look for the `event` field. Notable events: `startup`, `auth_disabled`, `clients_loaded`, `client_registered`, `client_updated`, `client_deregistered`, `client_expired`, `client_auto_deregistered`, `clients_reloaded`, `proxy_upstream_failed`, `proxy_invalid_client_url`, `breaker_opened`, `breaker_closed`, `http_request`, `server_failed`, `shutdown_complete`.

## Configuration

| Variable | Description | Default |
|----------|-------------|----------|
| `PORT` | Server port | `12001` |
| `PERSISTENCE_FILE` | JSON file for storing registered clients | `clients.json` |
| `API_KEY` | Legacy API key (full permissions) | None (auth off) |
| `API_KEYS` | Comma-separated keys; `key:ro` marks a read-only key | None |
| `RATE_LIMIT_PER_MINUTE` | Requests per minute per key/IP (0 disables) | `0` |
| `HEALTH_CHECK_PATH` | Path probed on downstreams | `/status` |
| `HEALTH_CHECK_INTERVAL` | Probe interval, Go duration (e.g. `30s`); 0 disables | `0` |
| `HEALTH_CHECK_ON_REGISTER` | Reject registrations of unreachable clients | `false` |
| `HEALTH_CHECK_THRESHOLD` | Consecutive failures before a client is marked unhealthy | `3` |
| `AUTO_DEREGISTER` | Remove clients that exceed the health threshold | `false` |
| `FILE_RELOAD_INTERVAL` | How often to check the persistence file for external changes (0 disables) | `0` |
| `HTTPS_CERT` / `HTTPS_KEY` | Serve TLS directly when both are set | None |

### Status Codes

The proxy endpoints use these codes in addition to the usual ones:

- `400` - missing/invalid `client_id`, `path`, or JSON body; invalid URL; unreachable client with `HEALTH_CHECK_ON_REGISTER`
- `401` - missing or wrong `X-API-Key`
- `403` - read-only key used on a write endpoint
- `404` - unknown or expired client
- `413` - request body over 1 MiB on management endpoints
- `429` - rate limit exceeded (includes `Retry-After`)
- `502` - all upstream candidates unreachable
- `503` - circuit breaker open for that client

### Security

**API keys**: Set `API_KEY` (single key) or `API_KEYS=k1,k2:ro` (multiple keys; the `:ro` suffix grants read-only access - listing clients and proxying only). All endpoints, including `/clients`, `/metrics`, and `/audit`, require a key when authentication is configured. Without any key the proxy is open to everyone who can reach it - setting one is strongly recommended.

**Rate limiting**: Keyed by API key fingerprint (or client IP when auth is off). Applies to every endpoint; responses include `Retry-After`.

**Credential exposure**: Downstream credentials are stored in the persistence file but are **never returned** by the API - `/clients` always returns an empty `password` field.

**Client persistence**: Registered clients are saved to `clients.json` (or `PERSISTENCE_FILE`) and loaded at startup. The file is written atomically with `0600` permissions. When `FILE_RELOAD_INTERVAL` is set, external edits to the file are detected and reloaded (edits made by the service itself do not trigger a reload).

**Request validation**: Upstream URLs are validated at registration. Proxied paths must not contain `..` segments; management bodies are capped at 1 MiB.

**Header hygiene**: Hop-by-hop headers (`Connection`, `Transfer-Encoding`, ...) are stripped in both directions when proxying; `Authorization` from callers is overridden with the registered credentials.

## Systemd Service

A ready-to-use unit file is included in the repository (`systemd/central-osctl-api.service`). It runs the service as a dynamic (non-root) user and stores client data under `/var/lib/central-osctl-api`.

1. **Build and install the binary**

   ```sh
   go build -o central-osctl-api
   sudo cp central-osctl-api /usr/local/bin/
   ```

2. **Install, enable, and start the service**

   ```sh
   sudo cp systemd/central-osctl-api.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now central-osctl-api
   sudo systemctl status central-osctl-api
   ```

3. **Set an API key (recommended)**

   Authentication is disabled while neither `API_KEY` nor `API_KEYS` is configured. Add one without editing the unit file:

   ```sh
   sudo systemctl edit central-osctl-api
   ```

   ```ini
   [Service]
   Environment=API_KEY=your-secret-key-here
   ```

   Then restart:

   ```sh
   sudo systemctl restart central-osctl-api
   ```

## Docker

```sh
docker build -t central-osctl-api .
docker run -p 12001:12001 -e API_KEY=secret -v $(pwd)/data:/data central-osctl-api
```

The image ships a `HEALTHCHECK` (probes `/healthz` on the default port via `wget`). If you change `PORT`, also set `-e HEALTHCHECK_PORT=<port>`.

## Development

### Requirements

- Go 1.22 or later

### Building and Running

```sh
go build -o central-osctl-api
export API_KEY=test-key PERSISTENCE_FILE=dev-clients.json
./central-osctl-api
```

### Testing

```sh
go test ./...
go test -race ./...
```

The suite covers registration, auth (single + multi-key, read-only), rate limiting, TTL expiry, tags, proxy failover, circuit breaker, health checks, TLS skip-verify, live reload, Prometheus metrics, audit, and the REST-style proxy - all via `httptest` against the real mux.

## Contributing

Contributions are welcome! Please open an issue or submit a pull request on GitHub.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
