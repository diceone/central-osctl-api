# Central OSCTL API

The `central-osctl-api` is a central orchestrator API that manages and interacts with multiple `osctl` APIs. It provides endpoints to register, deregister, and list `osctl` API clients, as well as proxy requests to the registered clients.

## Features

- **Register `osctl` API clients** with URL validation
- **Deregister `osctl` API clients**
- **List registered `osctl` API clients**
- **Proxy requests to `osctl` API clients** with query parameter filtering
- **Persistent client storage** via JSON file
- **Optional API key authentication** for secure access

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

Proxy a request to a specific `osctl` API client. Additional query parameters (except `client_id` and `path`) are forwarded to the target API.

```sh
curl -X GET "http://localhost:12001/proxy?client_id=client1&path=/ram"
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

**API Key Authentication**: When `API_KEY` is set, all `/register` and `/unregister` requests must include an `X-API-Key` header with the correct key.

**Client Persistence**: Registered clients are saved to `clients.json` (or the file specified by `PERSISTENCE_FILE`) and automatically loaded on startup. The file is created with `0600` permissions (owner read/write only).

**URL Validation**: Client API URLs are validated at registration time and must be valid HTTP or HTTPS URLs.

## Systemd Service

To run the `central-osctl-api` as a systemd service:

1. **Create a Systemd Unit File**

   ```sh
   sudo nano /etc/systemd/system/central-osctl-api.service
   ```

2. **Add the Following Configuration**

   ```ini
   [Unit]
   Description=Central OSCTL API Service
   After=network.target

   [Service]
   Type=simple
   ExecStart=/usr/local/bin/central-osctl-api
   Restart=on-failure
   Environment=GOMAXPROCS=4
   Environment=PORT=12001
   Environment=PERSISTENCE_FILE=/var/lib/central-osctl-api/clients.json
   Environment=API_KEY=your-secret-key-here

   [Install]
   WantedBy=multi-user.target
   ```

3. **Reload Systemd, Enable, and Start the Service**

   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable central-osctl-api
   sudo systemctl start central-osctl-api
   ```

4. **Check the Service Status**

   ```sh
   sudo systemctl status central-osctl-api
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

Before running tests, make sure to set up the environment variables required for the provider configuration.

```sh
export OSCTL_API_URL=http://your-osctl-api-url
export OSCTL_USERNAME=admin
export OSCTL_PASSWORD=password
```

Run the tests:

```sh
go test ./...
```

## Contributing

Contributions are welcome! Please open an issue or submit a pull request on GitHub.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
```

This `README.md` provides a comprehensive overview of the `central-osctl-api` project, including installation, usage, systemd service setup, and development instructions. Adjust the repository URL and other details as needed to match your actual setup.
