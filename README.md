# cwnd-warm-up-proxy

A Go proxy that pre-warms HTTP/2 connections to an upstream server to maintain a large TCP congestion window (cwnd) and maximize throughput when real requests arrive. First iteration targets a single upstream server that supports HTTP/2.

Refer to SPEC.md for the detailed technical specification.
EOF

cat > SPEC.md <<\"EOF\"
# Technical Specification: cwnd-warm-up-proxy v0.1

## Overview
- Purpose: Pre-warm HTTP/2 connections to an upstream server to keep a large cwnd for maximum throughput when real requests arrive.
- Scope: Single upstream target; HTTP/2 to upstream (TLS or h2c). Maintain a pool of persistent HTTP/2 client sessions. Generate dummy traffic to grow cwnd per connection until a per-connection target rate is reached; then switch to periodic bursts. Forward real client requests via the pre-warmed pool with priority over dummy traffic. Terminal TUI shows bandwidth and throughput metrics, updated every 500ms with a 10s sliding window.

## Configuration (in source)
- target:
  - host (string)
  - port (int)
  - protocol: "http2" only (for now)
  - tls (bool)
  - tls_insecure_skip_verify (bool, default false)
- pool:
  - pool_size (int)
  - bandwidth_mbps (float64)
  - warm_up_interval_ms (int)
  - warm_up_size_bytes (int)
  - warm_up_direction: enum {upload, download, both}
  - warm_up_upload_path (string; required if upload/both)
  - warm_up_upload_method: enum {POST, PUT}
  - warm_up_download_path (string; required if download/both)
  - warm_up_download_method: enum {GET}
  - download_supports_range (bool)
  - per_connection_dwell_ms (int)
  - fallback_upload_retry (bool)
- server:
  - port (int)
  - inbound_tls (bool)
  - inbound_h2c (bool)
  - read/write/idle timeouts (ms)

## Derived
- per_connection_target_bps = floor((bandwidth_mbps * 1_000_000) / pool_size)

## Architecture & Key Components
- Packages: net/http, golang.org/x/net/http2, golang.org/x/net/http2/h2c, crypto/tls, golang.org/x/time/rate, context, sync, time, io, net.
- Upstream dialing: TLS with ALPN ("h2") or h2c via net.Dial.
- Connection pool: multiple *http2.ClientConn sessions over distinct TCP connections.
- Dummy traffic workers:
  - UploadWorker (client→server): long-lived POST/PUT with io.Pipe; limiter-based pacing; continuous then burst.
  - DownloadWorker (server→client): GET (prefer Range) bursts; read fixed bytes and cancel; continuous only if needed.
- Priority scheduling: real requests pause warm-up; resume when real traffic completes.
- Proxy server: reverse proxy handler forwards inbound requests to upstream via selected session.
- Metrics & TUI: sliding window counters, sampled at 100ms; render every 500ms; show estimated bandwidth, dummy tx/rx, real tx/rx.

## Dummy Traffic Strategy (HTTP/2 compliant)
- Upload warm-up:
  - Persistent POST/PUT to warm_up_upload_path with Content-Type: application/octet-stream.
  - ContinuousWarm until sustained per-connection target throughput, then BurstWarm (warm_up_size_bytes every warm_up_interval_ms).
  - Retry on early termination (configurable).
- Download warm-up:
  - Prefer Range GETs to warm_up_download_path; read BurstSize bytes then cancel; fallback to plain GET if Range unsupported.

## Proxy Forwarding
- Strip hop-by-hop headers; stream request body upstream; stream response to client.
- Count bytes for metrics (RealTx, RealRx).
- Retry idempotent methods once on error.

## Metrics
- Global atomic counters: DummyTx, DummyRx, RealTx, RealRx.
- Sampling interval: 100ms; window: 10s.
- Render interval: 500ms; use ANSI clears; show per-session states.

## Concurrency & Shutdown
- Goroutines per session for workers and health; sampler; renderer; reconnection; HTTP server.
- Graceful shutdown via context and server.Shutdown; pause workers; close ClientConn/Conn.

## Open Questions (to finalize before implementation)
1. Warm-up direction: upload, download, or both?
2. Upstream endpoints:
   - Upload sink path (POST/PUT) that accepts arbitrary bytes until client closes.
   - Download source path supporting Range (preferred) or large payload.
3. Authentication/headers needed for dummy traffic?
4. Upstream constraints (rate limits, MaxConcurrentStreams)?
5. TLS certificate verification vs self-signed (testing).

## Risks & Mitigations
- Precise downstream control requires Range; otherwise overshoot possible.
- Upstream may early-terminate POST/PUT; implement retry/backoff.
- GOAWAY/limits: track health, reconnect, modulate concurrency.
- Warmed detection heuristics may be imperfect; use dwell and thresholds.

## Verification Plan
- Run with upload warm-up to a configured sink path; observe TUI.
- Issue real requests through proxy; observe priority and metrics.
- Simulate upstream connection loss; verify reconnection.
EOF

git add .
GIT_AUTHOR_NAME=Rowan\\ Stein GIT_AUTHOR_EMAIL=rowan.stein@agyn.io GIT_COMMITTER_NAME=Rowan\\ Stein GIT_COMMITTER_EMAIL=rowan.stein@agyn.io git commit -m "Initialize repository with README and technical SPEC (v0.1)"

# Create GitHub repo and push
gh repo create agyn-sandbox/cwnd-warm-up-proxy --private --source=. --push -y

# Create Issue #1
cat > /tmp/ISSUE_BODY.md <<\"EOF\"
# Implement cwnd-warm-up-proxy v0.1

This issue tracks the first implementation of the cwnd-warm-up-proxy as specified in SPEC.md.

## Summary
- Pre-warm a pool of HTTP/2 connections to a single upstream server (TLS or h2c).
- Generate dummy traffic (upload/download) to grow cwnd to a per-connection target rate, then maintain with bursts.
- Forward real requests with priority over warm-up traffic.
- Render terminal metrics (updated every 500ms; 10s window): estimated bandwidth, dummy tx/rx, real tx/rx.

## Configuration Keys
See SPEC.md for the full configuration. Minimum required:
- target.host, target.port, target.tls
- pool.pool_size, pool.bandwidth_mbps, pool.warm_up_interval_ms, pool.warm_up_size_bytes
- pool.warm_up_direction and corresponding warm-up paths
- server.port

## Open Questions (blocking implementation details)
1. Warm-up direction(s): upload, download, or both?
2. Upstream warm-up endpoints:
   - Upload sink path (POST/PUT) that accepts arbitrary bytes until client closes.
   - Download source path (GET), ideally supports Range requests.
3. Authentication requirements for dummy traffic (headers/tokens)?
4. Upstream constraints (rate limits, MaxConcurrentStreams)?
5. TLS certificate verification vs self-signed (for testing).

## Plan
- Once the above questions are answered, proceed to implement according to SPEC.md in a single branch and open a PR linked to this issue.

## Links
- SPEC: https://github.com/agyn-sandbox/cwnd-warm-up-proxy/blob/main/SPEC.md
EOF

gh issue create -R agyn-sandbox/cwnd-warm-up-proxy -t "Implement cwnd-warm-up-proxy v0.1" --body-file /tmp/ISSUE_BODY.md
