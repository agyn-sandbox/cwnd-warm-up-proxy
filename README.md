# SOCKS Overlay Proxy (striped multi-connection TCP)

This repository now targets a SOCKS5 client/server overlay that stripes a single logical TCP stream across multiple prewarmed TLS/TCP connections between client and server, reassembles at the server, and forwards to the target as a single TCP stream.

- Client: local SOCKS5 proxy used by the consumer
- Server: near the target (can be on the same host)
- Goal: improve throughput on the consumer→server leg using multiple prewarmed connections

See SPEC.md for the detailed technical specification (v0.1).
