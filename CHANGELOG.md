# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project uses semantic versioning.

## [Unreleased]

### Added

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
