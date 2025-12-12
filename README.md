# cwnd-warm-up-proxy

A Go reverse proxy that pre-warms HTTP/2 connections to an upstream origin so bursty uploads can immediately reuse large congestion windows. Each pool connection streams dummy upload traffic until it reaches a configured bandwidth, then maintains the rate with periodic bursts. Real client requests pause the warm-up flow on their session, forward over existing HTTP/2 streams, and resume warm-up once complete. A terminal UI renders bandwidth and throughput metrics using a 10-second sliding window.

See [SPEC.md](SPEC.md) for the authoritative technical specification.

## Features

- Upload-only warm-up using persistent POST/PUT requests with configurable dummy headers.
- HTTP/2 connection pool with per-connection dwell, burst interval, and rate limiting.
- Reverse proxy that accepts HTTP/1.1 and optional h2c inbound traffic; forwards over HTTP/2 with hop-by-hop header stripping and one retry for idempotent methods.
- Global metrics sampled every 100 ms with a 10 s window and ANSI TUI refreshed every 500 ms.
- Automatic reconnection on GOAWAY/EOF and graceful shutdown support.

## Configuration

Provide a JSON configuration file via `--config`:

```json
{
  "target": {
    "host": "upstream.local",
    "port": 443,
    "protocol": "http2",
    "tls": true
  },
  "pool": {
    "pool_size": 4,
    "bandwidth_mbps": 800,
    "warm_up_interval_ms": 500,
    "warm_up_size_bytes": 1048576,
    "warmup_path": "/upload/sink",
    "warmup_method": "POST",
    "per_connection_dwell_ms": 1000,
    "warm_up_headers": {
      "X-Warmup": "true"
    }
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
