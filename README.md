# SOCKS Overlay Proxy (striped multi-connection TCP)

This repository now targets a SOCKS5 client/server overlay that stripes a single logical TCP stream across multiple prewarmed TLS/TCP connections between client and server, reassembles at the server, and forwards to the target as a single TCP stream.

- Client: local SOCKS5 proxy used by the consumer
- Server: near the target (can be on the same host)
- Goal: improve throughput on the consumer→server leg using multiple prewarmed connections

## Features

- Selective acknowledgements with retransmission to recover quickly from loss.
- Bounded reorder buffers and credit-based flow control to cap memory growth under duress.
- Adaptive weighted scheduler driven by recent goodput to prioritise healthy subflows.
- Automatic subflow re-dial on the client when a transport path drops.

See SPEC.md for the detailed technical specification (v0.1).

## Prerequisites

- Go 1.22.7 (set `GOTOOLCHAIN=go1.22.7`).
- TLS keypair for the overlay server (self-signed certificates are fine for local testing).

## Quick start

1. **Generate a TLS certificate (local testing):**

   ```bash
   openssl req -x509 -nodes -newkey rsa:2048 \
     -keyout server.key -out server.crt -days 7 -subj '/CN=localhost'
   ```

2. **Start the overlay server:**

   ```bash
   go run ./cmd/server \
     -listen :8443 \
     -cert server.crt \
     -key server.key \
     -subflows 6
   ```

   The server accepts TLS subflows, reconstructs streams, and forwards traffic to the requested TCP targets.

3. **Start the SOCKS5 client:**

   ```bash
   go run ./cmd/client \
     -server 127.0.0.1:8443 \
     -listen 127.0.0.1:1080 \
     -subflows 6
   ```

   The client establishes `K` TLS subflows (default `6`), prewarms them, and exposes a local SOCKS5 endpoint on `127.0.0.1:1080`.

4. **Configure a tool or browser to use the SOCKS proxy.** For example:

   ```bash
   curl --socks5 127.0.0.1:1080 https://example.com/
   ```

## No-TLS quickstart (local testing)

For loopback experiments you can disable TLS on the overlay. The transport falls
back to plaintext TCP and automatically enables CRC32C checksums on every
frame.

1. **Start the test target** (if not already running):

   ```bash
   go run ./cmd/test-target -listen 127.0.0.1:9000
   ```

2. **Start the overlay server in plaintext mode**:

   ```bash
   go run ./cmd/server \
     -listen 127.0.0.1:8443 \
     -tls=false \
     -subflows 4
   ```

3. **Start the overlay client without TLS**:

   ```bash
   go run ./cmd/client \
     -server 127.0.0.1:8443 \
     -listen 127.0.0.1:1080 \
     -subflows 4 \
     -tls=false
   ```

4. **Send a direct upload** (reusing a known payload path):

   ```bash
   FILE=/tmp/test-consumer-plaintext.txt
   go run ./cmd/test-consumer \
     -target http://127.0.0.1:9000/upload \
     -file "$FILE"
   ```

5. **Send the same file through SOCKS**:

   ```bash
   go run ./cmd/test-consumer \
     -target http://127.0.0.1:9000/upload \
     -use-socks \
     -socks 127.0.0.1:1080 \
     -file "$FILE"
   ```

## Live metrics dashboards

Each binary now ships with a lightweight terminal dashboard that refreshes every
500 ms over a 10 second sliding window. Real bytes represent stream payload, and
dummy bytes cover heartbeats, probes, frame headers, and retransmissions.

- **Overlay client & server** — enabled by default (`-tui=true`). The dashboard
  lists per-subflow transmit/receive rates split into real/dummy bytes alongside
  aggregate totals and active stream throughput. Disable with `-tui=false` if a
  quiet console is preferred.

  ```bash
  go run ./cmd/server -listen :8443 -cert server.crt -key server.key -tui
  go run ./cmd/client -server 127.0.0.1:8443 -listen 127.0.0.1:1080 -tui
  ```

- **test-consumer** — shows live upload progress (bytes sent, average/instant
  throughput, server response) with dummy bytes fixed at zero. Toggle via the
  `-tui` flag.

  ```bash
  go run ./cmd/test-consumer -target http://127.0.0.1:9000/upload -tui
  ```

- **test-target** — reports bytes received, request rates, and the most recent
  upload size/duration with dummy bytes fixed at zero. Toggle via `-tui`.

  ```bash
  go run ./cmd/test-target -listen 127.0.0.1:9000 -tui
  ```

Dashboards emit to stdout, so run each binary in its own terminal when
observability is needed.

## Full local walkthrough (test harness)

The repository ships two helper binaries to exercise the overlay end-to-end:

- `cmd/test-target`: a simple HTTP service that records uploads.
- `cmd/test-consumer`: a CLI that pushes a 5 MiB text payload either directly to the target or through the SOCKS proxy and reports timings.

Follow the steps below in separate terminals:

1. **Start the test target (HTTP)**

   ```bash
   go run ./cmd/test-target -listen 127.0.0.1:9000
   ```

   The service exposes `POST /upload` and `GET /health`. Leave it running.

2. **(Once) Generate a TLS cert for the overlay server**

   ```bash
   openssl req -x509 -nodes -newkey rsa:2048 \
     -keyout server.key -out server.crt -days 7 -subj '/CN=localhost'
   ```

3. **Start the overlay server near the target**

   ```bash
   go run ./cmd/server \
     -listen 127.0.0.1:8443 \
     -cert server.crt \
     -key server.key \
     -subflows 4
   ```

4. **Start the overlay client/SOCKS proxy near the consumer**

   ```bash
   go run ./cmd/client \
     -server 127.0.0.1:8443 \
     -listen 127.0.0.1:1080 \
     -subflows 4
   ```

5. **Run the consumer *directly* to the target**

   ```bash
   go run ./cmd/test-consumer -target http://127.0.0.1:9000/upload
   ```

   The CLI creates a 5 MiB text file (path printed in the log), pushes it to
   the target, and prints timings such as:

   ```text
   2025/01/10 12:00:01 generated payload file: /tmp/test-consumer-12345.txt
   2025/01/10 12:00:02 mode: direct
   2025/01/10 12:00:02 client_time_ms=842
   2025/01/10 12:00:02 throughput_mibps=5.87
   2025/01/10 12:00:02 server_bytes=5242880 server_time_ms=410
   ```

6. **Run the consumer through the SOCKS proxy**

   Reuse the generated file to keep inputs identical:

   ```bash
   FILE=/tmp/test-consumer-12345.txt # from the direct run output
   go run ./cmd/test-consumer \
     -target http://127.0.0.1:9000/upload \
     -use-socks \
     -socks 127.0.0.1:1080 \
     -file "$FILE"
   ```

   The mode line now reads `mode: SOCKS 127.0.0.1:1080`; compare timings with
   the direct run.

### Troubleshooting

- Ensure all binaries are built/run with Go 1.22.7.
- If the consumer reports `connection refused`, verify the overlay client and
  server are running and that the server certificate matches the `CN` used.
- Use `curl http://127.0.0.1:9000/health` to confirm the target service is up.
- Terminate background processes with `Ctrl+C` after testing.

## Testing

Run the full test suite (unit + integration) with:

```bash
go test ./...
```

Static analysis:

```bash
go vet ./...
```

The integration test provisions an overlay client/server pair and transfers a 5 MiB payload through a local echo target to validate striping, flow-control, and reassembly behaviour.
