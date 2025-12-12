# SOCKS Overlay v0.1 — Technical Specification (Prototype)

## Overview
Client stripes across K TLS/TCP subflows; server reassembles and forwards to target TCP. TCP only; SOCKS5 CONNECT.

## Protocol
Frames: DATA, ACK, CTRL, HEARTBEAT. Headers include version, type, flags, session_id, stream_id, seq_no, len, optional checksum.

## Flow Control
Credit-based sliding window; bounded reorder buffer; conservative duplication after ~3×RTT.

## Scheduling
Adaptive WRR by recent goodput; frame size defaults 32 KiB (adaptive 8–64 KiB); pacing token bucket.

## SOCKS
Client does full SOCKS5 handshake; server opens target TCP.

## Config
Client defaults: K=6, TLS, frame=32KiB, inflight≈512KiB, heartbeat=5s, prewarm=2s; SOCKS listen 127.0.0.1:1080. Server: listen 443, TLS cert/key.

## Metrics
10s window, 500ms render; per-subflow RTT/goodput; per-stream throughput/outstanding.

## Failure Handling
Stall detection/replacement; heartbeat; bounded reorder buffers; clean shutdown.

