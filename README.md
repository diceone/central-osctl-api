# Central OSCTL API

The `central-osctl-api` is a central orchestrator API that manages and interacts with multiple `osctl` APIs. It provides endpoints to register, deregister, and list `osctl` API clients, as well as proxy requests to the registered clients.

## Features

- **Register `osctl` API clients** with URL validation
- **Deregister `osctl` API clients**
- **List registered `osctl` API clients**
- **Proxy requests to `osctl` API clients** with query parameter filtering
- **Crash-safe persistent client storage** via JSON file (atomic write + rename)
- **Optional API key authentication** on all endpoints (`/register`, `/unregister`, `/clients`, `/proxy`)

## Installation

### Prerequisites

- Go 1.20 or later

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

   The server will run on port `12001` by default:

   ```sh
   curl http://localhost:12001/clients
   ```

## Usage

### Register a Client

Register a new `osctl` API client. If API key authentication is enabled, include the `X-API-Key` header.

```sh
curl -X POST \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{"id": "client1", "api_url": "http://localhost:12000", "username": "admin", "password": "password"}' \
  http://localhost:12001/register
```

**Note**: The `api_url` must be a valid HTTP or HTTPS URL. Invalid URLs will be rejected at registration time.

### Deregister a Client

Deregister an existing `osctl` API client.

```sh
curl -X POST \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key" \
  -d '{"id": "client1"}' \
  http://localhost:12001/unregister
```

### List Clients

List all registered `osctl` API clients.

```sh
curl http://localhost:12001/clients
```

### Proxy a Request

Proxy a request to a specific `osctl` API client. If API key authentication is enabled, include the `X-API-Key` header. Additional query parameters (except `client_id` and `path`) are forwarded to the target API; parameters already present in the registered `api_url` act as defaults and can be overridden by the proxied request.

```sh
curl -H "X-API-Key: your-secret-key" "http://localhost:12001/proxy?client_id=client1&path=/ram"
```

Example with additional query parameters:

```sh
curl -X GET "http://localhost:12001/proxy?client_id=client1&path=/ram&sort=asc&limit=10"
# Forwards to: http://localhost:12000/ram?sort=asc&limit=10
```

## Configuration

The `central-osctl-api` can be configured using environment variables:

| Variable | Description | Default |
|----------|-------------|----------|
| `PORT` | Server port | `12001` |
| `PERSISTENCE_FILE` | JSON file for storing registered clients | `clients.json` |
| `API_KEY` | API key for authentication (optional) | None (authentication disabled) |

### Security

**API Key Authentication**: Set `API_KEY` to require an `X-API-Key` header on **every** endpoint, including `/clients` and `/proxy`. Without a key, authentication is disabled and anyone who can reach the server can register clients and route requests through the proxy — setting a key is strongly recommended.

**Credential Exposure**: Downstream credentials are stored in the persistence file but are **never returned** by the API. The `/clients` endpoint always returns an empty `password` field.

**Client Persistence**: Registered clients are saved to `clients.json` (or the file specified by `PERSISTENCE_FILE`) and automatically loaded on startup. The file is created with `0600` permissions (owner read/write only) and updated atomically (write to a temp file, then rename), so a crash cannot corrupt it.

**Request Validation**: Client API URLs are validated at registration time and must be valid HTTP or HTTPS URLs with a host. Proxied paths must not contain `..` segments, and request bodies on management endpoints are capped at 1 MiB.

**Header Hygiene**: Hop-by-hop headers (`Connection`, `Transfer-Encoding`, …) are stripped in both directions when proxying.

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

   Authentication is disabled while no `API_KEY` is configured. Add one without editing the unit file:

   ```sh
   sudo systemctl edit central-osctl-api
   ```

   Add the following (use a strong secret — **do not commit real keys** to the repository):

   ```ini
   [Service]
   Environment=API_KEY=your-secret-key-here
   ```

   Then restart:

   ```sh
   sudo systemctl restart central-osctl-api
   ```

## Development

### Requirements

- Go 1.20 or later

### Building the Project

Clone the repository and build the project:

```sh
git clone https://github.com/diceone/central-osctl-api.git
cd central-osctl-api
go build -o central-osctl-api
```

### Running the Project

Run the project locally:

```sh
./central-osctl-api
```

With configuration:

```sh
export API_KEY=test-key
export PERSISTENCE_FILE=dev-clients.json
./central-osctl-api
```

### Testing

Run the test suite (no external configuration required):

```sh
go test ./...
```

## Contributing

Contributions are welcome! Please open an issue or submit a pull request on GitHub.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
