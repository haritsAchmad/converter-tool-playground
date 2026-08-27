# Roadmap

## 0.2 — operational hardening

- [x] Persist minimal job state for restart-safe status reporting.
- [x] Per-IP token-bucket rate limiting and upload quotas.
- [x] Prometheus metrics, request IDs, and OpenTelemetry hooks.
- [x] Layered upload gate: extension chain, magic bytes, MIME, syntax, and active-script rejection.
- [x] Separate API and conversion worker containers with a Redis-backed queue and shared job storage.
- [x] Optional ClamAV stage before the conversion queue; fail closed when enabled.
- [x] Harden the currently-supported structured-data pipeline against known injection/DoS classes: CSV cells are now quote-escaped against formula/DDE injection (OWASP "CSV Injection"), and regression tests confirm the YAML and JSON decoders already reject alias bombs and pathologically deep nesting instead of exhausting resources.
- [ ] Format-specific content-disarm stage for DOCX/PDF/ODT once those land in 0.3—today's formats (structured data, images, Markdown/HTML) don't have that attack surface (macros, embedded objects, external references) yet, so this is blocked on 0.3 rather than actionable now.
- [x] Optional shared-secret `X-API-Key` authentication on the API and `/metrics`, with the web UI prompting for and remembering the key.
- [x] Documented reverse-proxy config (TLS termination) for deployments reachable beyond localhost, since the API key alone doesn't protect traffic in transit.

## 0.3 — more engines

- [x] PDF → PNG/JPEG (first page, fixed DPI) via poppler's `pdftoppm`, gated behind a second, independent structural validation pass (pdfcpu) so a hostile PDF has to fool two different parsers, not one.
- [ ] Move PDF conversion into its own isolated worker/queue instead of the shared worker pool, for real blast-radius containment if the renderer is ever exploited—deliberately deferred as a lighter first slice; revisit once there's a reason to believe the shared pool isn't enough.
- [ ] Multi-page PDF output (currently first-page-only, since a job produces exactly one output file today) and PDF as an output format.
- [ ] DOCX/ODT/PDF via LibreOffice and Pandoc with macros, network, and risky coders disabled.
- [ ] Audio/video through FFmpeg with probe limits and codec whitelists.
- [ ] SVG only after a dedicated sanitizer and rasterization boundary.

## Later

- S3-compatible temporary storage with bucket lifecycle rules.
- Accounts, quotas, history, and PostgreSQL.
- Webhooks/API keys and multi-instance deployment.

Format additions are gated on a threat model, deterministic resource limits, corpus tests, and a maintained upstream engine—not just whether a tool can technically open the file.
