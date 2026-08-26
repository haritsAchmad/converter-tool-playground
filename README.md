# Convertbox

Small, self-hosted file conversion service with deliberately short-lived storage. Upload a file, choose a supported output, optionally rename it, and download the result before it is automatically removed.

## Supported conversions

| Family | Formats | Notes |
|---|---|---|
| Structured data | CSV, JSON, XML, YAML | CSV output requires an array of flat objects. XML uses a deterministic generic representation. |
| Images | PNG, JPEG/JPG, WebP | PNG↔JPEG is native Go. WebP appears only when ImageMagick is installed. Metadata is stripped on ImageMagick conversions. |
| Documents | Markdown, HTML | Markdown output is a best-effort semantic conversion. |

The API returns capabilities at runtime, so unavailable engines are not advertised. Office/PDF/audio/video are intentionally not enabled in this first release; see [ROADMAP.md](ROADMAP.md).

## Quick start

With Docker (recommended):

```sh
docker compose up --build
```

Open <http://localhost:8080>. The compose profile runs as UID 10001 with a read-only root filesystem, drops all Linux capabilities, prevents privilege escalation, and limits CPU, memory, PIDs, and temporary storage.

Run locally with Go 1.24+:

```sh
go run ./cmd/convertbox
```

ImageMagick 7 is optional locally and enables WebP when `magick` is on `PATH`. For a public deployment, run behind Caddy/Nginx with HTTPS, rate limiting, and an aligned request body limit.

## Configuration

| Variable | Default | Purpose |
|---|---:|---|
| `CONVERTBOX_ADDR` | `:8080` | HTTP listen address |
| `CONVERTBOX_STORAGE` | OS temp + `convertbox` | Isolated job root |
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

Durations use Go syntax such as `30s` and `10m`.

## API

- `GET /api/v1/formats` — capabilities and limits
- `POST /api/v1/jobs` — multipart fields: `file`, `outputFormat`, optional `outputName`; rate-limited and quota-limited per IP
- `GET /api/v1/jobs/{uuid}` — job status
- `GET /api/v1/jobs/{uuid}/download` — completed output
- `GET /healthz` — liveness
- `GET /metrics` — Prometheus metrics (HTTP and job counters/histograms); not authenticated, so keep it off public ingress or scrape it internally

Every response carries an `X-Request-ID` header (echoed back if the caller supplies a well-formed one) for correlating logs.

Example:

```sh
curl -F file=@people.csv -F outputFormat=json -F outputName=people \
  http://localhost:8080/api/v1/jobs
```

## Security model

- Extension, detected MIME, magic bytes, and syntax/header validation are combined; filenames alone are never trusted.
- Executable/script extensions and common executable signatures are rejected.
- When `CONVERTBOX_CLAMSCAN` is configured, uploads must pass ClamAV before entering the conversion queue. Detection rejects the upload; scanner errors and timeouts fail closed.
- Uploads are streamed into mode `0600` UUID job directories (mode `0700`) under one normalized storage root.
- Original names are metadata only. Server-generated paths are used for input and output; rename input is reduced to a safe basename.
- External tools use `exec.CommandContext` with separate fixed arguments, a fixed working directory, and a minimal environment—never shell concatenation.
- Queue length, workers, body size, request time, and job time are bounded. Container runtime limits provide hard CPU/RAM/PID/storage boundaries.
- Per-IP token-bucket rate limiting and a concurrent-job quota bound submission abuse from a single client; the client IP is read from the raw TCP connection, not from forwardable headers like `X-Forwarded-For`.
- Cleanup verifies that targets are descendants of the converter root and refuses symlink job directories. Old orphan directories are removed after restart.
- Job state is persisted to a `job.json` sidecar per job so status/downloads survive a restart. Jobs still in flight at shutdown are recovered as failed rather than silently resumed.
- Logs contain job ID, formats, size, worker, and errors—not user file contents.
- Downloads use `nosniff`, attachment disposition, and an opaque content type.

This reduces risk; it does not make arbitrary hostile document processing safe. Keep conversion workers isolated from credentials and internal networks. For a public multi-tenant service, split API and workers into separate containers/VMs and add malware scanning, rate limits, quotas, and abuse controls.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/convertbox
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md) before submitting changes or security reports.

## License

MIT — see [LICENSE](LICENSE).
