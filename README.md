# Central OSCTL API

The `central-osctl-api` is a central orchestrator API that manages and interacts with multiple `osctl` APIs. It provides endpoints to register, deregister, and list `osctl` API clients, as well as proxy requests to the registered clients.

## Features

- **Register `osctl` API clients**
- **Deregister `osctl` API clients**
- **List registered `osctl` API clients**
- **Proxy requests to `osctl` API clients**

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

1. **Run the Central API**

   ```sh
   ./central-osctl-api
   ```

2. **Verify the Server is Running**

   The server will run on port `12001` by default:

   ```sh
   curl http://localhost:12001/clients
   ```

## Usage

### Register a Client

Register a new `osctl` API client.

```sh
curl -X POST -H "Content-Type: application/json" -d '{"id": "client1", "api_url": "http://localhost:12000", "username": "admin", "password": "password"}' http://localhost:12001/register
```

### Deregister a Client

Deregister an existing `osctl` API client.

```sh
curl -X POST -H "Content-Type: application/json" -d '{"id": "client1"}' http://localhost:12001/unregister
```

### List Clients

List all registered `osctl` API clients.

```sh
curl http://localhost:12001/clients
```

### Proxy a Request

Proxy a request to a specific `osctl` API client.

```sh
curl -X GET "http://localhost:12001/proxy?client_id=client1&path=/ram"
```

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
