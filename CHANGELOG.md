# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project uses semantic versioning.

## [Unreleased]

### Fixed

- CSV output (from JSON/XML/YAML input) now quote-escapes cells starting with `=`, `+`, `-`, `@`, tab, or CR before writing them, closing an OWASP "CSV Injection" hole: an attacker-controlled field value like `=cmd|' /C calc'!A0` used to reach the exported `.csv` byte-for-byte, ready to run as a formula/DDE command the moment someone opened it in Excel, Sheets, or LibreOffice. Added regression tests, plus tests confirming (they already passed without a code change) that the YAML and JSON decoders reject alias bombs and deeply nested input instead of hanging or exhausting memory.
- `compose.yaml`: the `redis` service dropped all Linux capabilities but the official `redis:8-alpine` entrypoint needs `CAP_SETUID`/`CAP_SETGID` to drop from root to its `redis` (999:1000) user before writing to `/data`. Without those caps it silently kept running as root, which then lacked `CAP_DAC_OVERRIDE` to write into the image's `999:1000`-owned data directory, so `redis-server` failed to create `appendonlydir` and the whole stack refused to start. Fixed by pinning `user: "999:1000"` on the service so it starts as the right identity directly, bypassing the entrypoint's privilege-drop path entirely.

### Added

- Optional `caddy` reverse-proxy service (`docker compose --profile proxy up`) for TLS termination in front of `api`: [deploy/Caddyfile](deploy/Caddyfile) covers both a public-domain (automatic Let's Encrypt) and an internal/LAN (self-signed, on-demand per-hostname) setup, and blocks `/metrics` on the public listener. Documented in the README's new "Deploying beyond localhost" section, including the trade-off that fronting the API with a proxy collapses per-IP rate limiting into one shared bucket (Caddy's container IP), since `clientIP` deliberately ignores `X-Forwarded-For`.
- Optional shared-secret authentication: when `CONVERTBOX_API_KEY` is set, every `/api/v1/*` and `/metrics` request must send it back as `X-API-Key` (compared in constant time) or gets `401`. `/healthz` and the static web UI stay open. The bundled web UI now prompts for the key on a `401`, remembers it in `localStorage`, and sends it on every request—including downloads, which switched from a plain `<a href>` link to a `fetch` + blob so the header can actually be attached.
- Restart-safe job state: each job persists a `job.json` sidecar next to its files, and the server rebuilds its in-memory job map from disk on startup so status/download keep working after a restart. Jobs still queued or processing at shutdown are recovered as failed since they cannot be resumed.
- Per-IP token-bucket rate limiting on job submission (`CONVERTBOX_RATE_RPS`, `CONVERTBOX_RATE_BURST`) and a concurrent-job quota per IP (`CONVERTBOX_MAX_JOBS_PER_IP`).
- Request IDs (`X-Request-ID`) on every response, structured access logging, Prometheus metrics at `GET /metrics`, and OpenTelemetry HTTP instrumentation wired to the global (no-op by default) tracer provider.
- Initial Go server and embedded responsive web interface.
- Async bounded worker pool with UUID job isolation, deadlines, polling, and downloads.
- CSV, JSON, XML, YAML, PNG, JPEG, Markdown, and HTML conversion.
- Optional ImageMagick-powered WebP support with a restrictive policy.
- Multi-layer upload validation and executable/script rejection.
- Automatic TTL and orphan cleanup constrained to the storage root.
- Non-root, read-only Docker deployment with resource limits.
- Tests, CI, security policy, contributing guide, and roadmap.
