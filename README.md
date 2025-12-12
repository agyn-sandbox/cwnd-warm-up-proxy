# cwnd-warm-up-proxy

A Go reverse proxy that pre-warms HTTP/2 connections to an upstream origin so bursty uploads can immediately reuse large congestion windows. Each pool connection streams dummy upload traffic until it reaches a configured bandwidth, then maintains the rate with periodic bursts. Real client requests pause the warm-up flow on their session, forward over existing HTTP/2 streams, and resume warm-up once complete. A terminal UI renders bandwidth and throughput metrics using a 10-second sliding window.

See [SPEC.md](SPEC.md) for the authoritative technical specification.

## Quickstart

### 1. Launch the upload test server

```bash
go run ./cmd/testserver -port 9000
```

The test server listens on HTTP/1.1 and h2c simultaneously. Leave it running while you experiment; press <kbd>Ctrl</kbd>+<kbd>C</kbd> (or send SIGINT/SIGTERM) for a graceful shutdown with final metrics. Flag defaults (such as the port and h2c toggle) live in [`cmd/testserver/main.go`](cmd/testserver/main.go).

### 2. Create a proxy configuration

Save the following as `config.json`:

```json
{
  "target": {
    "host": "localhost",
    "port": 9000,
    "protocol": "http2",
    "tls": false
  },
  "pool": {
    "pool_size": 2,
    "bandwidth_mbps": 100,
    "warm_up_interval_ms": 1000,
    "warm_up_size_bytes": 1048576,
    "warmup_path": "/warmup-upload",
    "warmup_method": "POST",
    "warm_up_headers": {},
    "per_connection_dwell_ms": 0
  },
  "server": {
    "port": 8080,
    "support_http1_1": true,
    "support_h2c": true
  }
}
```

This sample points the proxy at the upload test server. [`internal/config/config.go`](internal/config/config.go) normalises headers and derives defaults (for example, choosing h2c automatically for cleartext targets). Adjust that file if you need different default behaviour when fields are omitted.

### 3. Start the warm-up proxy

```bash
go run ./cmd/proxy --config ./config.json
```

Run this in a second terminal while the test server is active. Logs will show warm-up progress and upload metrics.

### 4. Exercise the proxy

- Open [http://localhost:8080/](http://localhost:8080/) in a browser to use the HTML upload form.
- Upload a file via cURL:

  ```bash
  curl -F file=@/path/to/file http://localhost:8080/upload
  ```

Traffic flows through the proxy to the test server, which streams the file and updates its TUI dashboard.

## Features

- Upload-only warm-up using persistent POST/PUT requests with configurable dummy headers.
- HTTP/2 connection pool with per-connection dwell, burst interval, and rate limiting.
- Reverse proxy that accepts HTTP/1.1 and optional h2c inbound traffic; forwards over HTTP/2 with hop-by-hop header stripping and one retry for idempotent methods.
- Global metrics sampled every 100 ms with a 10 s window and ANSI TUI refreshed every 500 ms.
- Automatic reconnection on GOAWAY/EOF and graceful shutdown support.

## Configuration

Provide a JSON configuration file via `--config` (the Quickstart example above is a ready-to-run template):

```json
{
  "target": {
    "host": "localhost",
    "port": 9000,
    "protocol": "http2",
    "tls": false
  },
  "pool": {
    "pool_size": 2,
    "bandwidth_mbps": 100,
    "warm_up_interval_ms": 1000,
    "warm_up_size_bytes": 1048576,
    "warmup_path": "/warmup-upload",
    "warmup_method": "POST",
    "per_connection_dwell_ms": 0,
    "warm_up_headers": {}
  },
  "server": {
    "port": 8080,
    "support_http1_1": true,
    "support_h2c": true
  }
}
```

`bandwidth_mbps` and `pool_size` determine each connection’s target bandwidth (in bits per second). TLS verification of the upstream origin is skipped by default as required for v0.1.

## Running

```bash
go build ./cmd/proxy
./proxy --config ./config.json
```

The TUI clears the terminal and shows the sliding-window duration, estimated upload bandwidth (dummy + real), dummy transmit rate, and real transmit/receive rates, followed by each session’s warm-up phase, status, connection health, and last error (if any).

### Requirements

- Go 1.22 (toolchain 1.22.7 or newer). The module declares `toolchain go1.22.7`; run commands with a Go binary that honours toolchain downloads or install Go 1.22 locally.

## Testing

```bash
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
```

Unit tests cover warm-up pause/resume behaviour, session prioritisation for real traffic, and metrics window/formatting logic. Additional integration testing should validate dummy traffic pacing and TUI output against real upstream targets.

## Upload Test Server

A standalone upload test server lives under [`cmd/testserver`](cmd/testserver) for validating upstream handling.

- `GET /` serves a simple HTML form for selecting a file to upload.
- `POST /upload` streams multipart file data to completion and reports the received size plus time-to-receive.

While running, the server renders a terminal dashboard (refresh ~500 ms) with a 10-second sliding window showing total uploads, inbound bandwidth (bytes/s and Mbps), and the latest upload’s size/duration. Start it with:

```bash
go run ./cmd/testserver -port 8081
```

Flags such as `-require_h2` (force HTTP/2 clients) and `-h2c=false` (disable cleartext HTTP/2) help exercise different inbound protocols.
