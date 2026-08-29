# Convertbox

Small, self-hosted file conversion service with deliberately short-lived storage. Upload a file, choose a supported output, optionally rename it, and download the result before it is automatically removed.

## Supported conversions

| Family | Formats | Notes |
|---|---|---|
| Structured data | CSV, JSON, XML, YAML | CSV output requires an array of flat objects. XML uses a deterministic generic representation. |
| Images | PNG, JPEG/JPG, WebP | PNG↔JPEG is native Go. WebP appears only when ImageMagick is installed. Metadata is stripped on ImageMagick conversions. |
| Documents | Markdown, HTML | Markdown output is a best-effort semantic conversion. |
| Office | DOCX, XLSX, PPTX → PDF | Uses isolated, headless LibreOffice profiles. Complex Microsoft-specific layout may render differently. |
| PDF | PDF → PNG/JPEG | Renders the first page only, at a fixed 150 DPI, via poppler's `pdftoppm`; appears only when it's installed. Full-document rendering and PDF as an output format are future work. |

The API returns capabilities at runtime, so unavailable engines are not advertised. PDF-to-Office, legacy Office formats, macro-enabled documents, audio, and video are intentionally not enabled; see [ROADMAP.md](ROADMAP.md).

## Quick start

With Docker (recommended):

```sh
docker compose up --build
```

Open <http://localhost:8080>. Compose starts separate API and conversion-worker containers, a Redis-backed queue, and a shared job volume. Application containers run as UID 10001 with read-only root filesystems, drop all Linux capabilities, prevent privilege escalation, and limit CPU, memory, and PIDs.

Run locally with Go 1.24+:

```sh
go run ./cmd/convertbox
```

ImageMagick 7 is optional locally and enables WebP when `magick` is on `PATH`; poppler-utils likewise enables PDF→image when `pdftoppm` is on `PATH`; and LibreOffice enables DOCX/XLSX/PPTX→PDF when `libreoffice` or `soffice` is on `PATH`. The Docker image includes all three. For anything reachable beyond localhost, see [Deploying beyond localhost](#deploying-beyond-localhost) below before you open it up.

## Configuration

| Variable | Default | Purpose |
|---|---:|---|
| `CONVERTBOX_ADDR` | `:8080` | HTTP listen address |
| `CONVERTBOX_MODE` | `standalone` | `standalone`, `api`, or `worker` process role |
| `CONVERTBOX_STORAGE` | OS temp + `convertbox` | Isolated job root |
| `CONVERTBOX_REDIS_URL` | empty | Redis URL; required in `api` and `worker` modes |
| `CONVERTBOX_REDIS_QUEUE` | `convertbox:jobs` | Redis queue key prefix |
| `CONVERTBOX_MAX_MB` | `25` | Per-upload limit |
| `CONVERTBOX_WORKERS` | `2` | Concurrent conversions |
| `CONVERTBOX_QUEUE_SIZE` | `20` | Bounded waiting queue |
| `CONVERTBOX_JOB_TIMEOUT` | `45s` | Per-job deadline |
| `CONVERTBOX_JOB_TTL` | `20m` | Retention after completion |
| `CONVERTBOX_CLEANUP_INTERVAL` | `1m` | Cleanup sweep interval |
| `CONVERTBOX_UPLOAD_TIMEOUT` | `30s` | Server read deadline |
| `CONVERTBOX_RATE_RPS` | `1` | Per-IP job submission rate (tokens/sec) |
| `CONVERTBOX_RATE_BURST` | `5` | Per-IP token bucket burst size |
| `CONVERTBOX_MAX_JOBS_PER_IP` | `4` | Max concurrent (queued/processing) jobs per IP |
| `CONVERTBOX_CLAMSCAN` | disabled | Path or command name for an optional `clamscan` executable |
| `CONVERTBOX_SCAN_TIMEOUT` | `15s` | Per-upload antivirus scan deadline |
| `CONVERTBOX_API_KEY` | disabled | When set, every `/api/v1/*` and `/metrics` request must send it back as `X-API-Key`; the bundled web UI prompts for it and remembers it in the browser. Leave unset for local/trusted-network use |

Durations use Go syntax such as `30s` and `10m`.

## API

- `GET /api/v1/formats` — capabilities and limits
- `POST /api/v1/jobs` — multipart fields: `file`, `outputFormat`, optional `outputName`; rate-limited and quota-limited per IP
- `GET /api/v1/jobs/{uuid}` — job status
- `GET /api/v1/jobs/{uuid}/download` — completed output
- `GET /healthz` — liveness
- `GET /metrics` — Prometheus metrics (HTTP and job counters/histograms); not authenticated, so keep it off public ingress or scrape it internally

Every response carries an `X-Request-ID` header (echoed back if the caller supplies a well-formed one) for correlating logs.

When `CONVERTBOX_API_KEY` is set, every `/api/v1/*` and `/metrics` request needs an `X-API-Key` header matching it, or it gets `401`; `/healthz` and the static web UI stay open so the page can load and prompt for the key.

Example:

```sh
curl -F file=@people.csv -F outputFormat=json -F outputName=people \
  -H "X-API-Key: $CONVERTBOX_API_KEY" \
  http://localhost:8080/api/v1/jobs
```

## Security model

- Extension, detected MIME, magic bytes, and syntax/header validation are combined; filenames alone are never trusted.
- CSV output quote-escapes cells that open with `=`, `+`, `-`, `@`, tab, or CR, so a converted value can't be interpreted as a formula/DDE command when opened in a spreadsheet (OWASP "CSV Injection"). The YAML and JSON decoders reject alias bombs and pathologically deep nesting outright rather than exhausting memory or the stack.
- PDF input must clear two independent parsers before conversion: a magic-byte check, then a full structural validation pass with pdfcpu (pure Go, no cgo)—separate from the native `pdftoppm` renderer that actually touches the file afterward, so a PDF crafted to exploit one specific parser's bug is much less likely to also cleanly validate against the other. Rendering is capped to the first page at a fixed DPI and bounded by the same job timeout as everything else.
- Executable/script extensions and common executable signatures are rejected.
- OOXML input must be a plausible ZIP package of the matching family. Entry count, expanded size, compression ratio, and paths are bounded; macros, ActiveX, embedded objects, and external non-hyperlink resources are rejected before LibreOffice. Each conversion uses an ephemeral LibreOffice profile and the job deadline.
- When `CONVERTBOX_CLAMSCAN` is configured, uploads must pass ClamAV before entering the conversion queue. Detection rejects the upload; scanner errors and timeouts fail closed.
- Uploads are streamed into mode `0600` UUID job directories (mode `0700`) under one normalized storage root.
- Original names are metadata only. Server-generated paths are used for input and output; rename input is reduced to a safe basename.
- External tools use `exec.CommandContext` with separate fixed arguments, a fixed working directory, and a minimal environment—never shell concatenation.
- Queue length, workers, body size, request time, and job time are bounded. Container runtime limits provide hard CPU/RAM/PID/storage boundaries.
- Per-IP token-bucket rate limiting and a concurrent-job quota bound submission abuse from a single client; the client IP is read from the raw TCP connection, not from forwardable headers like `X-Forwarded-For`.
- Cleanup verifies that targets are descendants of the converter root and refuses symlink job directories. Old orphan directories are removed after restart.
- Job state is persisted to a `job.json` sidecar per job so status/downloads survive a restart. Jobs still in flight at shutdown are recovered as failed rather than silently resumed.
- Split mode passes only opaque job UUIDs through Redis. API and worker share the isolated job volume; Redis keeps unacknowledged work in a processing list so a single restarted worker service can requeue it.
- Logs contain job ID, formats, size, worker, and errors—not user file contents.
- Downloads use `nosniff`, attachment disposition, and an opaque content type.
- Optional shared-secret `X-API-Key` gate (`CONVERTBOX_API_KEY`) on the whole API and `/metrics`, compared in constant time; disabled by default since a lone-user local instance has no one else to authenticate.

This reduces risk; it does not make arbitrary hostile document processing safe. Keep conversion workers isolated from credentials and sensitive internal networks. The bundled worker is resource-limited but still shares a Redis network; a public multi-tenant service should give document conversion a dedicated, egress-denied sandbox/container and add malware scanning, rate limits, quotas, and abuse controls.

The Redis recovery model currently assumes one worker service (which may run several configured worker goroutines). Do not scale the worker service to multiple replicas until per-worker leases and stale-claim recovery are implemented.

## Deploying beyond localhost

By default `docker compose up` binds `api` to `0.0.0.0:8080`—reachable from your LAN, not just this machine, if your firewall allows inbound connections. Before letting anyone but you reach it:

1. **Set `CONVERTBOX_API_KEY`.** Without it, there is no authentication at all; a job's UUID is the only thing standing between a stranger and its output.
2. **Put a TLS-terminating proxy in front of it.** Nothing in Convertbox itself speaks HTTPS. Bring one up with:

   ```sh
   CONVERTBOX_API_KEY=$(openssl rand -hex 32) docker compose --profile proxy up --build -d
   ```

   This starts a `caddy` service on ports 80/443 in addition to the usual stack; see [deploy/Caddyfile](deploy/Caddyfile) for both the public-domain (automatic Let's Encrypt) and internal/LAN (self-signed, on-demand) variants, and edit it to match your setup before relying on it. `api`'s own `8080:8080` mapping stays open too—drop it from `compose.yaml` once the proxy is your only intended entry point.

3. **Know what a proxy does to the built-in rate limiting.** `CONVERTBOX_RATE_RPS`/`CONVERTBOX_RATE_BURST`/`CONVERTBOX_MAX_JOBS_PER_IP` key off the raw TCP peer address on purpose (a client can't spoof `X-Forwarded-For` to dodge its own limit). That also means once every request arrives via Caddy, every client shares one bucket—Caddy's container IP—instead of getting their own. For a small internal team this is usually a fine trade-off (worst case, one heavy user throttles the others, not an outage); it does mean the per-IP quotas stop being a meaningful abuse control once you're behind the proxy, so lean on the API key for that instead.
4. **Keep `/metrics` off the public listener.** The bundled Caddyfile already 404s it; if you write your own proxy config, do the same; and don't skip `CONVERTBOX_API_KEY`, which also gates `/metrics` directly.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/convertbox
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md) before submitting changes or security reports.

## License

MIT — see [LICENSE](LICENSE).
