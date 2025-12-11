# Technical Specification: cwnd-warm-up-proxy v0.1 (updated)

## Confirmed Scope and Clarifications
- Warm-up direction: upload-only (proxy → upstream server).
- Dummy traffic headers: configurable key/value headers applied to warm-up requests.
- Real traffic: forward real client headers to upstream (strip hop-by-hop as usual).
- Streams: do not artificially limit per-connection concurrent streams beyond configuration; open as many as configured (warm-up uses one persistent stream per connection; real requests create streams as needed).
- TLS to upstream: no certificate verification required; default behavior is to skip verification (no additional config flag needed).
- Inbound proxy: support both HTTP/1.1 and HTTP/2 (h2c for cleartext; TLS inbound optional, but not required for v0.1).

## Configuration (in source)
- target:
  - host (string)
  - port (int)
  - protocol: "http2" only
  - tls (bool)
- pool:
  - pool_size (int)
  - bandwidth_mbps (float64)
  - warm_up_interval_ms (int)
  - warm_up_size_bytes (int)
  - warmup_path (string) — upload sink endpoint
  - warmup_method: enum {POST, PUT} — upload method
  - warm_up_headers: map[string]string — headers applied only to dummy warm-up requests
  - per_connection_dwell_ms (int)
- server:
  - port (int)
  - support_http1_1 (bool) — default true
  - support_h2c (bool) — default true when TLS inbound is false
  - read/write/idle timeouts (ms)

## Derived
- per_connection_target_bps = floor((bandwidth_mbps * 1_000_000) / pool_size)

## Architecture & Key Components
- Packages: net/http, golang.org/x/net/http2, golang.org/x/net/http2/h2c, crypto/tls, golang.org/x/time/rate, context, sync, time, io, net.
- Upstream dialing: TLS with ALPN ("h2") using tls.Config{InsecureSkipVerify: true} or h2c via net.Dial.
- Connection pool: multiple *http2.ClientConn sessions over distinct TCP connections.
- Dummy traffic worker (upload-only):
  - Persistent POST/PUT to warmup_path with Content-Type: application/octet-stream.
  - Apply warm_up_headers on the request.
  - ContinuousWarm until sustained per-connection target throughput, then BurstWarm (warm_up_size_bytes every warm_up_interval_ms).
  - Retry on early termination.
- Priority scheduling: real requests pause warm-up; resume when real traffic completes.
- Proxy server: reverse proxy handler forwards inbound requests (HTTP/1.1 or HTTP/2) to upstream via selected session.
- Metrics & TUI: sliding window counters, sampled at 100ms; render every 500ms; show estimated bandwidth, dummy tx, real tx/rx.

## Proxy Forwarding
- Strip hop-by-hop headers; forward real headers as-is; stream request body; stream response to client.
- Count bytes for metrics (RealTx, RealRx).
- Retry idempotent methods once on error.

## Concurrency & Shutdown
- Goroutines per session for upload worker and health; sampler; renderer; reconnection; HTTP server.
- Graceful shutdown via context and server.Shutdown; pause workers; close ClientConn/Conn.

## Open Items (for completeness, not blocking v0.1)
- Authentication for dummy traffic: supply values via warm_up_headers if needed.
- Upstream constraints: none enforced by proxy beyond its own configuration; we will try to open streams as configured.

## Verification Plan
- Run with upload warm-up to configured warmup_path; observe TUI reaching target per-connection rate; switch to bursts.
- Issue real requests through proxy; observe dummy pause and real throughput metrics.
EOF

git add SPEC.md
GIT_AUTHOR_NAME=Rowan\\ Stein GIT_AUTHOR_EMAIL=rowan.stein@agyn.io GIT_COMMITTER_NAME=Rowan\\ Stein GIT_COMMITTER_EMAIL=rowan.stein@agyn.io git commit -m "Update SPEC with user clarifications: upload-only, warmup_path/method, dummy headers, TLS skip verify, inbound HTTP/1.1 + h2c"

# Set remote and push
if ! git remote | grep -q ^origin; then
  git remote add origin https://github.com/agyn-sandbox/cwnd-warm-up-proxy.git
fi
# Ensure main branch name
current_branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$current_branch" != "main" ]; then
  git branch -M main
fi
git push -u origin main

# Create Issue #1
cat > /tmp/ISSUE_BODY.md <<"EOF"
# Implement cwnd-warm-up-proxy v0.1 (upload-only)

This issue tracks the first implementation of the cwnd-warm-up-proxy as specified in SPEC.md, with upload-only warm-up.

## Confirmed Requirements
- Warm-up direction: upload-only using a persistent POST/PUT to the configured `warmup_path`.
- Dummy traffic headers: configurable `warm_up_headers` applied to warm-up requests.
- Real traffic: forward real client headers to upstream (strip hop-by-hop).
- Streams: open as many streams as configured; do not artificially limit beyond configuration.
- TLS to upstream: skip certificate verification by default (no explicit insecure flag).
- Inbound proxy: support both HTTP/1.1 and HTTP/2 (h2c); TLS inbound optional but not required for v0.1.

## Configuration Keys (minimum)
- target.host, target.port, target.tls
- pool.pool_size, pool.bandwidth_mbps, pool.warm_up_interval_ms, pool.warm_up_size_bytes
- pool.warmup_path, pool.warmup_method (POST or PUT), pool.warm_up_headers (map)
- server.port

## Implementation Plan (single branch & PR)
1. Initialize Go module and project structure (`cmd/proxy`, `internal/...`).
2. Implement configuration types and validation.
3. Build upstream HTTP/2 connection pool (TLS or h2c) with `http2.ClientConn`.
4. Implement UploadWorker: continuous warm-up to target per-connection rate, then interval bursts; apply `warm_up_headers` on dummy requests; priority pause/resume with real traffic.
5. Implement reverse proxy server that accepts HTTP/1.1 and HTTP/2 (h2c), forwards requests via the pool; copy headers and bodies; hop-by-hop stripping; idempotent retry.
6. Implement metrics counters, sampler (100ms), and TUI renderer (500ms) over a 10s window.
7. Add graceful shutdown, error handling, reconnection.
8. Local testing: confirm warm-up behavior, priority handling, metrics rendering.

## Links
- SPEC: https://github.com/agyn-sandbox/cwnd-warm-up-proxy/blob/main/SPEC.md
EOF

gh issue create -R agyn-sandbox/cwnd-warm-up-proxy -t "Implement cwnd-warm-up-proxy v0.1 (upload-only)" --body-file /tmp/ISSUE_BODY.md
