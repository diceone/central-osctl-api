### Step 1: Create the Systemd Unit File

Create a new file named `central-osctl-api.service` in the `/etc/systemd/system/` directory.

```sh
sudo nano /etc/systemd/system/central-osctl-api.service
```

### Step 2: Add the Unit File Configuration

Add the following configuration to the `central-osctl-api.service` file:

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

### Explanation

- **[Unit]**:
  - `Description`: A brief description of the service.
  - `After`: Specifies the order of start. This service will start after the network target is reached.

- **[Service]**:
  - `Type=simple`: The service will be considered started right after the `ExecStart` command is executed.
  - `ExecStart`: The command to start the service. Adjust the path to where your `central-osctl-api` binary is located.
  - `Restart=on-failure`: Configures the service to restart automatically on failure.
  - `Environment`: Sets environment variables for the service. Adjust as needed.

- **[Install]**:
  - `WantedBy=multi-user.target`: Makes the service start on multi-user run levels.

### Step 3: Reload Systemd, Enable, and Start the Service

1. **Reload Systemd**: Reload the systemd manager configuration to apply the new unit file.

   ```sh
   sudo systemctl daemon-reload
   ```

2. **Enable the Service**: Enable the service to start on boot.

   ```sh
   sudo systemctl enable central-osctl-api
   ```

3. **Start the Service**: Start the service immediately.

   ```sh
   sudo systemctl start central-osctl-api
   ```

4. **Check the Service Status**: Verify that the service is running.

   ```sh
   sudo systemctl status central-osctl-api
   ```

### Step 4: Place the Binary in the Appropriate Location

Ensure that the `central-osctl-api` binary is placed in `/usr/local/bin/` or adjust the `ExecStart` path in the unit file to the location of your binary.

For example:

```sh
sudo mv central-osctl-api /usr/local/bin/
sudo chmod +x /usr/local/bin/central-osctl-api
```

