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
