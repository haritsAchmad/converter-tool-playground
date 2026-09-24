# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project uses semantic versioning.

## [Unreleased]

### Added

- Optional `pdfMode` (`standard` default, or `optimized`) on Office→PDF jobs, surfaced as a selector in the web UI when a DOCX/XLSX/PPTX targets PDF. `optimized` sets LibreOffice's `SinglePageSheets` (Calc: every sheet forced onto one page), `EmbedStandardFonts`, and `ReduceImageResolution`/`MaxImageResolution=150` export filter options; `standard` passes none, unchanged from before this field existed. Filter names and option semantics verified against LibreOffice's own filter registry and documentation, not guessed. Also added this codebase's first regression tests that actually run a real DOCX/XLSX through LibreOffice (previously only OOXML package-structure validation was tested, never an actual conversion)—PPTX coverage stayed at the filter-logic level; see ROADMAP.md for why.
- PDF → PNG/JPEG now renders every page (previously first-page-only), up to a new 300-page validation cap, packaged as a ZIP with one `page-N.<ext>` entry per page. This is a **breaking change** to the output shape for this pair: it used to be a bare image and is now always a ZIP, even for a one-page source, so the API/UI contract stays predictable regardless of page count. Extension resolution for the new `.zip` output is now the single `outputExtension(in, out)` helper shared by job creation *and* `store.reload()`—the latter previously re-derived the expected extension from `formats[out].Extensions[0]` on its own and rejected every PDF→PNG/JPEG job's `.zip` `OutputName` outright, which made both the status endpoint and the worker treat the job as unavailable and silently leave it stuck at `queued` forever instead of converting it. `reload()` also falls back to the pre-ZIP `.png`/`.jpg` extension (`legacyOutputExtensions`) for a job that already **completed** under the old convention, so an already-finished job's status/download stay reachable for the rest of its retention window instead of breaking the moment the new convention shipped—new jobs are unaffected, they still only ever get `.zip`. Added store-level and full HTTP (create→worker→status/download, and separately status+download for a seeded legacy-format completed job) regression tests for both the original bug and the compatibility fix.
- PNG/JPEG/WebP → PDF, wrapping the source image as a single PDF page sized to it at 150 DPI. Implemented with pdfcpu (already a dependency for PDF structural validation) rather than a new external tool, so it's always available.
- Runtime-detected DOCX, XLSX, and PPTX to PDF conversion through headless LibreOffice.
- Bounded OOXML package validation that rejects macros, ActiveX, embedded objects, traversal paths, and external non-hyperlink resources before conversion.

### Changed

- Office conversions use private temporary LibreOffice profiles, and the container includes common metric-compatible fonts.

### Fixed

- Image upload validation now checks the declared width/height from the PNG/JPEG header against a 100-megapixel ceiling before conversion ever decodes the full pixel buffer. Previously `image.DecodeConfig`'s dimensions were read but discarded, so a small file could declare an arbitrarily large image and force a multi-gigabyte allocation on decode—a classic decompression bomb bounded only loosely by the job timeout and container memory limit (i.e., an OOM-kill, not a clean rejection). Added regression tests for both the oversized-dimension rejection and that ordinary images still pass.
- CSV output (from JSON/XML/YAML input) now quote-escapes cells starting with `=`, `+`, `-`, `@`, tab, or CR before writing them, closing an OWASP "CSV Injection" hole: an attacker-controlled field value like `=cmd|' /C calc'!A0` used to reach the exported `.csv` byte-for-byte, ready to run as a formula/DDE command the moment someone opened it in Excel, Sheets, or LibreOffice. Added regression tests, plus tests confirming (they already passed without a code change) that the YAML and JSON decoders reject alias bombs and deeply nested input instead of hanging or exhausting memory.
- `compose.yaml`: the `redis` service dropped all Linux capabilities but the official `redis:8-alpine` entrypoint needs `CAP_SETUID`/`CAP_SETGID` to drop from root to its `redis` (999:1000) user before writing to `/data`. Without those caps it silently kept running as root, which then lacked `CAP_DAC_OVERRIDE` to write into the image's `999:1000`-owned data directory, so `redis-server` failed to create `appendonlydir` and the whole stack refused to start. Fixed by pinning `user: "999:1000"` on the service so it starts as the right identity directly, bypassing the entrypoint's privilege-drop path entirely.

### Added

- PDF → PNG/JPEG conversion (first page, fixed 150 DPI), via poppler's `pdftoppm`, appearing only when it's installed (bundled in the Docker image, optional locally). PDF input is validated by two independent parsers before conversion—a magic-byte check plus a full structural pass with the new `github.com/pdfcpu/pdfcpu` dependency (pure Go, no cgo)—separately from the native renderer that actually processes the file. Added tests covering acceptance of a well-formed PDF, rejection of a fake signature, rejection of a structurally broken PDF that has a real signature, and (skipped unless `pdftoppm` is installed) an actual render round-trip.
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
