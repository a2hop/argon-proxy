# Argon CORS Proxy

Simple proxy to work around cors and http mixed content


## Installation

### From Source

```bash
# Clone the repository
git clone https://github.com/a2hop/argon-proxy.git
cd argon-proxy

# Build the binary
go build -o argon-proxy .

# Install (optional)
sudo mv argon-proxy /usr/local/bin/
```

### Using the systemd Service

```bash
# Copy the systemd service file
sudo cp argon-proxy.service /etc/systemd/system/

# Reload systemd
sudo systemctl daemon-reload

# Enable and start the service
sudo systemctl enable argon-proxy.service
sudo systemctl start argon-proxy.service
```

## Usage

### Running the Proxy

```bash
# Run with default settings (listen on 127.0.0.1:8080)
argon-proxy

# Run with custom address and port
argon-proxy --address=0.0.0.0 --port=9000

# Enable verbose logging
argon-proxy --verbose
```

### Command-line Options

| Flag | Default | Description |
|------|---------|-------------|
| `--address` | `127.0.0.1` | Address to listen on |
| `--port` | `8080` | Port to listen on |
| `--allow-origin` | `*` | CORS Allow-Origin header value |
| `--verbose` | `false` | Enable verbose logging |
| `--trust-proxy` | `false` | Trust X-Forwarded-* headers |

### Making Proxy Requests

#### Using path format:

```
http://localhost:8080/proxy/https://api.example.com/data
```

#### Using query parameter:

```
http://localhost:8080/proxy/?target=https://api.example.com/data
```

### Accessing Configuration Files

List available configuration files:

```
http://localhost:8080/getconfig/
```

Retrieve a specific configuration file example:

```
http://localhost:8080/getconfig/nginx
```


## Systemd Service

The provided systemd service runs the proxy as an unprivileged user with security hardening options enabled.

To check the service status:

```bash
sudo systemctl status argon-proxy
```

To view logs:

```bash
sudo journalctl -u argon-proxy
```

## License

[MIT License](LICENSE)
