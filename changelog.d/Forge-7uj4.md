category: Security
- **Trusted-proxy client IP for audit logs** - The web audit log now records the direct peer address and only honours `X-Forwarded-For` when the peer is a configured trusted proxy (`FORGE_WEB_TRUSTED_PROXIES`), taking the rightmost untrusted hop, so a client can no longer forge the logged `remote` field. (Forge-7uj4)
- **Per-session SSE connection cap** - Server-Sent Events streams are now capped at 20 concurrent connections per web session, returning a friendly HTTP 429 beyond that, so many open tabs can no longer multiply DB/file pollers without bound. (Forge-7uj4)
