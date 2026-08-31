# Copilot Instructions: Central OSCTL API

## Project Overview

This is a **centralized orchestrator API** that manages multiple `osctl` API clients. It acts as a registry and proxy layer, allowing clients to register themselves and route requests through this central service to registered backend APIs.

**Core architecture**: Single-file Go HTTP server with in-memory state management - all logic lives in [main.go](../main.go).

## Quick Start

```bash
# 1. Build and start the server (with optional config)
export API_KEY="your-secret-key"  # Optional: Enable authentication
export PERSISTENCE_FILE="clients.json"  # Optional: Change persistence file
go build -o central-osctl-api && ./central-osctl-api

# 2. Register a client (in another terminal)
curl -X POST http://localhost:12001/register \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{"id":"client1","api_url":"http://localhost:8080","username":"admin","password":"secret"}'

# 3. List registered clients (requires X-API-Key when API_KEY is set; passwords are never returned)
curl -H "X-API-Key: your-secret-key" http://localhost:12001/clients

# 4. Proxy a request to the registered client
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/proxy?client_id=client1&path=/status"
```

## Key Components

- **CentralAPI struct**: Holds registered clients in a `map[string]OsctlClient` with mutex-protected concurrent access
- **OsctlClient struct**: Represents a downstream API with `ID`, `ApiURL`, `Username`, `Password` (Basic Auth)
- **Four HTTP endpoints** (each enforces its HTTP method and, when `API_KEY` is set, requires the `X-API-Key` header):
  - `POST /register` - Register new client (JSON body with all OsctlClient fields)
  - `POST /unregister` - Remove client (JSON body with `id` field); unknown IDs return 404
  - `GET /clients` - List all registered clients (returns JSON map; `password` always empty)
  - `GET /proxy?client_id=X&path=/endpoint` - Proxy request to registered client

**Example /clients response** (passwords are never exposed):
```json
{
  "client1": {
    "id": "client1",
    "api_url": "http://localhost:8080",
    "username": "admin",
    "password": ""
  },
  "client2": {
    "id": "client2",
    "api_url": "https://api.example.com",
    "username": "user",
    "password": ""
  }
}
```

## Development Workflow

### Building and Running Locally

```bash
go build -o central-osctl-api
./central-osctl-api  # Starts on port 12001 (configurable via PORT env)
```

Port defaults to `12001` but can be configured via `PORT` environment variable - see the end of [main.go](../main.go).

### Testing

```bash
go test ./...
```

Integration-style tests live in `main_test.go` (built with `httptest`), covering registration validation, authentication, password redaction on `/clients`, proxy forwarding/header handling, path traversal rejection, and persistence round-trips. Run with `-race` to check for data races.

### Docker Deployment

Multi-stage build using `golang:1.26-alpine` (pinned by digest) and a pinned Alpine runtime. The container runs as a non-root `app` user with `/data` as the working directory, so the default persistence file lives in `/data/clients.json`:
```bash
docker build -t central-osctl-api .

# Run with default settings
docker run -p 12001:12001 central-osctl-api

# Run with custom configuration
docker run -p 8080:8080 \
  -e PORT=8080 \
  -e API_KEY=secret-key \
  -e PERSISTENCE_FILE=/data/clients.json \
  -v $(pwd)/data:/data \
  central-osctl-api
```

See [Dockerfile](../Dockerfile) for the complete build process.

### Systemd Service

Production deployment uses systemd:

```bash
# 1. Build and install binary
go build -o central-osctl-api
sudo cp central-osctl-api /usr/local/bin/

# 2. Install systemd unit file
sudo cp systemd/central-osctl-api.service /etc/systemd/system/

# 3. Enable and start service
sudo systemctl daemon-reload
sudo systemctl enable central-osctl-api
sudo systemctl start central-osctl-api
sudo systemctl status central-osctl-api
```

Service runs with `GOMAXPROCS=4`, a dynamic non-root user, `StateDirectory=central-osctl-api` (`PERSISTENCE_FILE=/var/lib/central-osctl-api/clients.json`), and auto-restarts on failure. See [systemd/central-osctl-api.service](../systemd/central-osctl-api.service).

## Code Patterns

### Concurrency Safety

All client map access is protected with `api.mu.Lock()` / `defer api.mu.Unlock()` pattern. 

**Critical pattern in [main.go](../main.go#L147-L152)**: ProxyRequest locks to read client, then **unlocks before HTTP call** to avoid blocking concurrent requests:
```go
api.mu.Lock()
client, exists := api.clients[clientID]
api.mu.Unlock()  // Released BEFORE making HTTP request
```

All other endpoints hold lock for entire operation duration.

### HTTP Request Proxying

The `/proxy` endpoint:
- Extracts `client_id` and `path` from query parameters (rejects `..` path segments)
- Forwards request method, body, and headers to the downstream API (hop-by-hop headers are stripped in both directions; `Content-Length` is preserved)
- Uses Basic Auth credentials from registered client (set last, overriding any caller-supplied `Authorization` header)
- **Filters out** `client_id` and `path` before forwarding remaining query parameters; parameters registered in the client's `api_url` act as defaults and can be overridden
- Copies response status, headers, and body back to caller (hop-by-hop response headers stripped)

Example: `GET /proxy?client_id=client1&path=/ram&sort=asc` → proxies to `{client_api_url}/ram?sort=asc`

### Error Handling

Standard `http.Error()` responses with appropriate status codes - no custom error types or structured error responses.

**HTTP Status Codes**:
- `200 OK` - Successful register/unregister, successful proxy
- `400 Bad Request` - Missing/invalid client_id, path (`..` segments rejected), or JSON body; invalid URL format; empty client ID
- `401 Unauthorized` - Missing or incorrect X-API-Key header (when API_KEY is set); enforced on all endpoints
- `404 Not Found` - Client not found in proxy or unregister request
- `405 Method Not Allowed` - Wrong HTTP method for the endpoint (response includes an `Allow` header)
- `413 Request Entity Too Large` - Register/unregister body larger than 1 MiB
- `500 Internal Server Error` - JSON encoding errors, persistence write failures, or invalid stored client URL
- `502 Bad Gateway` - Downstream API request failure

## Configuration

**Environment Variables**:
- `PORT` - Server port (default: `12001`)
- `PERSISTENCE_FILE` - JSON file for client persistence (default: `clients.json`)
- `API_KEY` - Optional API key for authentication via `X-API-Key` header

**State Management**: 
- Clients are persisted to JSON file (default: `clients.json`) atomically via temp file + rename
- Loaded automatically on startup
- Saved after each successful register/unregister operation; a failed save returns 500 and rolls the in-memory map back

**Security Features**:
- API key authentication via `X-API-Key` header on every endpoint (when `API_KEY` is set), using constant-time comparison
- URL validation at registration time (must be valid http/https with a host)
- Query parameter filtering (removes `client_id` and `path` before proxying) and `..` path-segment rejection
- Hop-by-hop header stripping in both proxy directions
- 1 MiB request body cap on management endpoints
- Passwords are never returned by the API (`/clients` returns an empty `password` field)

## Critical Gotchas

⚠️ **File Permissions**: Persistence file is created with `0600` permissions (owner read/write only). Ensure process has write access to the directory.

⚠️ **Client Update**: No dedicated update endpoint - use register with same ID to update. This overwrites all fields.

⚠️ **Password Security**: Client passwords stored in plain text in the JSON file only. The `/clients` endpoint redacts them (`password` is always empty) - re-registering with the same ID is the only way to rotate a password. Keep the persistence file (0600) and its directory permissions tight.

⚠️ **No Health Checks**: Service doesn't verify if downstream APIs are reachable at registration time or periodically.

## Common Use Cases

### Update an Existing Client
Re-register with same `id` - all fields will be overwritten:
```bash
curl -X POST http://localhost:12001/register \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-key" \
  -d '{"id":"client1","api_url":"http://new-url:8080","username":"admin","password":"newpass"}'
```

### Backup/Restore Clients
```bash
# Backup
cp clients.json clients.backup.json

# Restore (stop service first)
sudo systemctl stop central-osctl-api
cp clients.backup.json clients.json
sudo systemctl start central-osctl-api
```

### Manual Client File Edit
```bash
# Edit clients.json directly (service must be stopped)
sudo systemctl stop central-osctl-api
vim /var/lib/central-osctl-api/clients.json
sudo systemctl start central-osctl-api
```

## Troubleshooting

**Problem**: Registration returns `401 Unauthorized`
- **Solution**: Check `X-API-Key` header matches `API_KEY` environment variable. Check server logs for "API Key authentication enabled".

**Problem**: Persistence file not saving
- **Solution**: Check directory permissions - the process needs write access to the directory. With the systemd unit, `StateDirectory=central-osctl-api` handles this automatically. Check logs for "Failed to persist clients" errors.

**Problem**: Registration returns `400 Bad Request: invalid api_url`
- **Solution**: Ensure URL includes scheme (`http://` or `https://`). Valid: `http://localhost:8080`, Invalid: `localhost:8080`.

**Problem**: Proxy request fails with `502 Bad Gateway`
- **Solution**: Check downstream API is reachable. Verify client credentials. Check logs for connection errors (details are logged server-side, not returned to callers).

**Problem**: Registration returns `405 Method Not Allowed`
- **Solution**: `/register` and `/unregister` require POST, `/clients` requires GET. Check the `Allow` response header for permitted methods.

**Problem**: Registration returns `413 Request Entity Too Large`
- **Solution**: Register/unregister request bodies are capped at 1 MiB. This limit (`maxRequestBodyBytes`) can be raised in [main.go](../main.go) if needed.

**Problem**: Clients lost after restart
- **Solution**: Check `PERSISTENCE_FILE` environment variable is set. Verify file exists and has correct permissions. Check startup logs for "Loaded N clients".

## Project Conventions

- **No dependencies**: Standard library only (`net/http`, `encoding/json`, `crypto/subtle`, `os`, `sync`, `time`)
- **Go version**: 1.25 specified in [go.mod](../go.mod)
- **State management**: JSON file persistence (default: `clients.json`)
- **No logging framework**: Uses `log` package for startup/warnings/errors
- **Configuration**: Environment variables only (`PORT`, `PERSISTENCE_FILE`, `API_KEY`)
- **Security**: Optional API key authentication via `X-API-Key` header

## Integration Points

This service coordinates with:
- **Downstream osctl APIs**: HTTP APIs that register themselves with this central service
- **Upstream consumers**: Services/users that query `/clients` and use `/proxy` to route requests

No message queues, databases, or external service dependencies.
