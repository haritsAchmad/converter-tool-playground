# Roadmap

## 0.2 — operational hardening

- [x] Persist minimal job state for restart-safe status reporting.
- [x] Per-IP token-bucket rate limiting and upload quotas.
- [x] Prometheus metrics, request IDs, and OpenTelemetry hooks.
- [x] Layered upload gate: extension chain, magic bytes, MIME, syntax, and active-script rejection.
- [ ] Separate API and conversion worker containers with a Redis-backed queue.
- [ ] Optional antivirus/content-disarm stage.

## 0.3 — more engines

- PDF transformations through a tightly sandboxed dedicated worker.
- DOCX/ODT/PDF via LibreOffice and Pandoc with macros, network, and risky coders disabled.
- Audio/video through FFmpeg with probe limits and codec whitelists.
- SVG only after a dedicated sanitizer and rasterization boundary.

## Later

- S3-compatible temporary storage with bucket lifecycle rules.
- Accounts, quotas, history, and PostgreSQL.
- Webhooks/API keys and multi-instance deployment.

Format additions are gated on a threat model, deterministic resource limits, corpus tests, and a maintained upstream engine—not just whether a tool can technically open the file.
