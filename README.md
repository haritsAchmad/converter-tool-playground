# Convertbox

Small, self-hosted file conversion service with deliberately short-lived storage. Upload a file, choose a supported output, optionally rename it, and download the result before it is automatically removed.

## Supported conversions

| Family | Formats | Notes |
|---|---|---|
| Structured data | CSV, JSON, XML, YAML | CSV output requires an array of flat objects. XML uses a deterministic generic representation. |
| Images | PNG, JPEG/JPG, WebP | PNG↔JPEG is native Go. WebP appears only when ImageMagick is installed. Metadata is stripped on ImageMagick conversions. PNG/JPEG/WebP → PDF is native Go (pdfcpu) and always available: one page, sized to the source image at 150 DPI. |
| Documents | Markdown, HTML | Markdown↔HTML is a best-effort semantic conversion. Markdown/HTML → PDF uses the same isolated, headless LibreOffice profiles as Office→PDF (Markdown is rendered to HTML first, via goldmark's default safe mode, then handed to LibreOffice); the resulting HTML is parsed and rejected rather than rendered if it contains `<script>`/active content, or any image/stylesheet/other resource reference other than an inline `data:` URI—not just an external URL, a local file path too, since LibreOffice would resolve it against the job's real files on disk. A `data:` URI itself is only accepted as one of a closed set of raster image formats (PNG/JPEG/GIF) whose declared type is cross-checked against its actual decoded bytes and size-bounded like any other image upload—not, say, a `data:text/css` stylesheet, whose own decoded content could still reference something external. An ordinary hyperlink is unaffected. Appears only when `libreoffice`/`soffice` is installed. |
| Office | DOCX, XLSX, PPTX → PDF | Uses isolated, headless LibreOffice profiles. Complex Microsoft-specific layout may render differently. Optional `pdfMode=optimized` (default `standard`, plain export) forces every Calc sheet onto one page, embeds standard fonts, and downsamples images to 150 DPI—see [Office → PDF fidelity modes](#office--pdf-fidelity-modes). |
| OpenDocument | ODT, ODS, ODP → PDF | Same isolated, headless LibreOffice profiles as Office→PDF, but without `pdfMode` (its fidelity controls were verified against OOXML's filter registry specifically, not ODF's, so it isn't offered for this pair). The package itself is validated like an OOXML upload—entry count/size and decompression-ratio bounds, path-traversal rejection, and Basic/Python macros and embedded `ObjectN/` subpackages rejected outright—plus a stricter check on `xlink:href` resource references than OOXML's own: rather than just blocking an *external* image/stylesheet reference, only a reference to a file that actually exists inside that same, already-validated package is accepted at all (an ordinary hyperlink is unaffected either way). |
| PDF | PDF → PNG/JPEG | Renders every page (up to 300) at a fixed 150 DPI via poppler's `pdftoppm`, packaged as a ZIP with one `page-N.png`/`page-N.jpg` entry per page—even for a one-page source; appears only when `pdftoppm` is installed. Rejects a PDF carrying an embedded JavaScript name tree, an embedded file attachment, or a document-level open/additional action—the mechanisms that fire the instant a tool opens the file, no interaction needed—as part of the same structural validation every PDF upload already goes through. PDF as an output format for structured-data sources is future work. |
| Audio | MP3, WAV, FLAC, Ogg (Vorbis/Opus) | Via `ffmpeg`/`ffprobe`; appears only when both are installed. Every upload is probed before conversion: rejected unless it has exactly one audio stream on a fixed per-container codec whitelist (ordinary embedded cover art is allowed, a real video/subtitle/data stream or a second audio stream isn't) and a duration under 4 hours. Both the probe and the actual conversion force the exact expected demuxer (`-f`) and whitelist only the `file` protocol, so a file merely *named* `.mp3` but structured as, say, an HLS playlist can't make ffmpeg fetch an internal URL or read an arbitrary local file—see [FFmpeg hardening](#ffmpeg-hardening). |
| Video | MP4 (H.264/AAC), WebM (VP8/VP9, Opus/Vorbis) | Same `ffmpeg`/`ffprobe` engine and hardening as audio (probe-then-convert, forced demuxer, `file`-only protocol whitelist), but deliberately narrower: exactly one video stream and at most one audio stream (silent video is fine; any subtitle/data stream, or more than one of either, is rejected outright—no cover-art-style exception here), a 720p pixel ceiling, and a 60-second duration cap. Both caps exist to keep a job plausibly finishable within this project's existing (audio/document-sized) job timeout rather than adding a video-specific one—size `CONVERTBOX_JOB_TIMEOUT` accordingly if you enable this, since actual encode time still depends on content complexity and available CPU, not just duration/resolution. |

The API returns capabilities at runtime, so unavailable engines are not advertised. PDF-to-Office, legacy Office formats, macro-enabled documents, and SVG are intentionally not enabled; see [ROADMAP.md](ROADMAP.md).

## Quick start

With Docker (recommended):

```sh
docker compose up --build
```

Open <http://localhost:8080>. Compose starts separate API, conversion-worker, and PDF-worker containers, a Redis-backed queue, and a shared job volume—PDF input is rendered by its own worker off its own queue, isolated from every other conversion (see [PDF worker isolation](#pdf-worker-isolation)). Application containers run as UID 10001 with read-only root filesystems, drop all Linux capabilities, prevent privilege escalation, and limit CPU, memory, and PIDs.

Run locally with Go 1.24+:

```sh
go run ./cmd/convertbox
```

ImageMagick 7 is optional locally and enables WebP when `magick` is on `PATH`; poppler-utils likewise enables PDF→image when `pdftoppm` is on `PATH`; LibreOffice enables DOCX/XLSX/PPTX→PDF, ODT/ODS/ODP→PDF, and Markdown/HTML→PDF when `libreoffice` or `soffice` is on `PATH`; and ffmpeg enables audio and video conversion when both `ffmpeg` and `ffprobe` are on `PATH`. The Docker image includes all four. For anything reachable beyond localhost, see [Deploying beyond localhost](#deploying-beyond-localhost) below before you open it up.

## Configuration

| Variable | Default | Purpose |
|---|---:|---|
| `CONVERTBOX_ADDR` | `:8080` | HTTP listen address |
| `CONVERTBOX_MODE` | `standalone` | `standalone`, `api`, `worker`, or `pdf-worker` process role—see [PDF worker isolation](#pdf-worker-isolation) for the last one |
| `CONVERTBOX_STORAGE` | OS temp + `convertbox` | Isolated job root |
| `CONVERTBOX_REDIS_URL` | empty | Redis URL; required in `api`, `worker`, and `pdf-worker` modes |
| `CONVERTBOX_REDIS_QUEUE` | `convertbox:jobs` | Redis queue key prefix for every job except PDF input |
| `CONVERTBOX_REDIS_PDF_QUEUE` | `convertbox:jobs:pdf` | Redis queue key for PDF input jobs, drained only by `pdf-worker` |
| `CONVERTBOX_MAX_MB` | `25` | Per-upload limit |
| `CONVERTBOX_WORKERS` | `2` | Concurrent conversions |
| `CONVERTBOX_QUEUE_SIZE` | `20` | Bounded waiting queue |
| `CONVERTBOX_MAX_JOB_ATTEMPTS` | `3` | Max times a worker will pick up the same job before marking it `failed` instead of requeuing it again; guards against a job that reliably crashes the worker process looping forever |
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
- `POST /api/v1/jobs` — multipart fields: `file`, `outputFormat`, optional `outputName`, optional `pdfMode` (`standard` or `optimized`, only accepted when converting an Office document to PDF—see [Office → PDF fidelity modes](#office--pdf-fidelity-modes)); rate-limited and quota-limited per IP
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

## Office → PDF fidelity modes

DOCX/XLSX/PPTX → PDF jobs accept an optional `pdfMode` field, and the bundled web UI shows a matching selector once a target format of PDF is chosen for an Office file:

| Mode | Behavior |
|---|---|
| `standard` (default) | Passes no LibreOffice export filter options at all—whatever page setup, fonts, and image fidelity the source document's own styles specify come through exactly as opening File → Export As PDF would produce, with nothing rewritten. |
| `optimized` | Sets `SinglePageSheets` (Calc only—forces every sheet onto exactly one PDF page regardless of its own print area/paper size), `EmbedStandardFonts` (avoids silent font substitution in the reader), and `ReduceImageResolution`/`MaxImageResolution=150` (smaller file, downsampled images). This is a deliberate, opt-in trade-off—layout can shift from what the source document would otherwise print as—never the default. |

`pdfMode` is rejected with `400` for any pair other than Office→PDF; it doesn't apply to image→PDF (that path is unrelated pure-Go code, not LibreOffice) or to PDF→image. It also doesn't apply to Markdown/HTML→PDF or ODT/ODS/ODP→PDF, even though both go through LibreOffice too—those filter options were verified specifically against OOXML's own filter registry entries, and nothing about a rendered HTML document or an ODF source document's own filter behavior was checked to make sure the same options apply there.

```sh
curl -F file=@report.xlsx -F outputFormat=pdf -F pdfMode=optimized \
  http://localhost:8080/api/v1/jobs
```

## Security model

- Extension, detected MIME, magic bytes, and syntax/header validation are combined; filenames alone are never trusted.
- CSV output quote-escapes cells that open with `=`, `+`, `-`, `@`, tab, or CR, so a converted value can't be interpreted as a formula/DDE command when opened in a spreadsheet (OWASP "CSV Injection"). The YAML and JSON decoders reject alias bombs and pathologically deep nesting outright rather than exhausting memory or the stack. PNG/JPEG input is checked against a 100-megapixel ceiling using the header's declared dimensions before any full decode, so a small file can't claim an enormous width/height and force a multi-gigabyte allocation (decompression bomb).
- PDF input must clear two independent parsers before conversion: a magic-byte check, then a full structural validation pass with pdfcpu (pure Go, no cgo)—separate from the native `pdftoppm` renderer that actually touches the file afterward, so a PDF crafted to exploit one specific parser's bug is much less likely to also cleanly validate against the other. That same validation pass also rejects anything over 300 pages, since rendering now covers every page (each becomes its own file before being zipped), not just the first. Rendering is capped to a fixed DPI and bounded by the same job timeout as everything else.
- Executable/script extensions and common executable signatures are rejected.
- Audio and video input are both probed with `ffprobe` before conversion and rejected unless their streams match a fixed per-container codec whitelist (video additionally: exactly one video stream, at most one audio stream, a resolution and duration cap, no other stream of any kind); every ffmpeg/ffprobe invocation forces the exact expected demuxer and whitelists only the `file` protocol, closing off the SSRF/local-file-disclosure class of ffmpeg exploit a crafted-but-media-extensioned file (an HLS playlist, a `concat` script) would otherwise open—see [FFmpeg hardening](#ffmpeg-hardening).
- OOXML input must be a plausible ZIP package of the matching family. Entry count, expanded size, compression ratio, and paths are bounded; macros, ActiveX, embedded objects, and external non-hyperlink resources are rejected before LibreOffice. Each conversion uses an ephemeral LibreOffice profile and the job deadline.
- When `CONVERTBOX_CLAMSCAN` is configured, uploads must pass ClamAV before entering the conversion queue. Detection rejects the upload; scanner errors and timeouts fail closed.
- Uploads are streamed into mode `0600` UUID job directories (mode `0700`) under one normalized storage root.
- Original names are metadata only. Server-generated paths are used for input and output; rename input is reduced to a safe basename.
- External tools use `exec.CommandContext` with separate fixed arguments, a fixed working directory, and a minimal environment—never shell concatenation.
- Queue length, workers, body size, request time, and job time are bounded. Container runtime limits provide hard CPU/RAM/PID/storage boundaries.
- Per-IP token-bucket rate limiting and a concurrent-job quota bound submission abuse from a single client; the client IP is read from the raw TCP connection, not from forwardable headers like `X-Forwarded-For`.
- Cleanup verifies that targets are descendants of the converter root and refuses symlink job directories. Old orphan directories are removed after restart.
- Job state is persisted to a `job.json` sidecar per job so status/downloads survive a restart. Jobs still in flight at shutdown are recovered as failed rather than silently resumed.
- Split mode passes only opaque job UUIDs through Redis. API, worker, and pdf-worker share the isolated job volume; each of the two separate Redis queues keeps its own unacknowledged work in its own processing list so its single restarted worker service can requeue it.
- PDF input is rendered by its own `pdf-worker` container off its own Redis queue (`CONVERTBOX_REDIS_PDF_QUEUE`), never the shared `worker` pool that handles every other conversion—see [PDF worker isolation](#pdf-worker-isolation).
- `worker` and `pdf-worker`—the two services that run an external tool against untrusted uploaded content—have no network route to the internet or the LAN at all in split mode, verified against a real `docker compose up`, not just config validation—see [Network egress denial](#network-egress-denial).
- Logs contain job ID, formats, size, worker, and errors—not user file contents.
- Downloads use `nosniff`, attachment disposition, and an opaque content type.
- Optional shared-secret `X-API-Key` gate (`CONVERTBOX_API_KEY`) on the whole API and `/metrics`, compared in constant time; disabled by default since a lone-user local instance has no one else to authenticate.

This reduces risk; it does not make arbitrary hostile document processing safe. `worker`/`pdf-worker` are resource-limited and network egress-denied (see [Network egress denial](#network-egress-denial)), but still share the job volume and Redis with each other; a public multi-tenant service should still keep conversion isolated from credentials and sensitive internal networks beyond that, add real sandboxing (gVisor/Kata-style, not just a container), and add malware scanning, rate limits, quotas, and abuse controls.

The Redis recovery model currently assumes one instance each of the `worker` and `pdf-worker` services (each of which may run several configured worker goroutines via `CONVERTBOX_WORKERS`). Do not scale either service to multiple replicas until per-worker leases and stale-claim recovery are implemented—`requeueActive()` unconditionally moves everything left in a queue's own processing list back onto that queue on startup, so a second replica of the same service restarting would requeue jobs the first replica is still actively working on.

## PDF worker isolation

A hostile PDF exploiting a bug in poppler's `pdftoppm` (the native renderer PDF→image conversion shells out to) would compromise whatever process ran it. In split mode, that process is `pdf-worker`, a container that does nothing except dequeue and render PDF→image jobs off `CONVERTBOX_REDIS_PDF_QUEUE`—not `worker`, which never touches PDF input at all and instead handles every other conversion (Office/ODF/Markdown/HTML→PDF via LibreOffice, images, structured data). A job's input format decides which queue it's enqueued on (`api`'s job creation), which decides which worker service ever sees it; the split is enforced in code, not by convention. `pdf-worker` also gets a lighter resource envelope than `worker` in the bundled `compose.yaml`, since it never has to run memory-hungry LibreOffice.

This is queue/process isolation, not a sandbox: `pdf-worker` still runs as the same non-root user, with the same read-only root filesystem and dropped capabilities as every other application container, sharing the same job volume as `worker`. It also can't reach the internet or the rest of the LAN at all—see [Network egress denial](#network-egress-denial) below. A compromised `pdf-worker` could still read/write other jobs' files on the shared volume or reach Redis and whatever else shares that same isolated network. Real blast-radius containment beyond that—a separate volume or storage credential, gVisor/Kata-style sandboxing—remains follow-up hardening; see [ROADMAP.md](ROADMAP.md).

`standalone` mode (no Redis, a single process) has no separate worker services to isolate PDF rendering into, so this split doesn't apply there—everything, PDF included, runs through the one local in-process queue, same as before this feature existed.

## Network egress denial

`worker` and `pdf-worker` are the two services that ever run an external tool (LibreOffice, ImageMagick, poppler's `pdftoppm`, ffmpeg/ffprobe) against untrusted uploaded file content. In split mode, both sit on `backend`, a Docker Compose network declared `internal: true`—which means, per [Docker's own network reference](https://docs.docker.com/reference/compose-file/networks/), the container gets no default gateway configured at all: there is no route out to the internet or the rest of the LAN, only to whatever else shares that same network (`redis`, their one real dependency). `api` is the one service that needs to stay reachable on its published port, so it's on both `backend` (to reach `redis`) and the normal `default` network (for the port itself)—a multi-homed container's published port keeps working on its non-internal network regardless of also holding an internal one.

This closes off the SSRF/local-file-disclosure/exfiltration class of risk this document already flags for a compromised converter process (see [FFmpeg hardening](#ffmpeg-hardening) for one concrete example of the kind of bug this backstops) at the network layer, not just the application layer: even a future bug in one of these tools that this project's own upload validation doesn't anticipate has nowhere to send data to or fetch from. Verified against a real `docker compose up`, not just `docker compose config`: from inside a running `worker`/`pdf-worker` container, `wget http://1.1.1.1` fails with "Network unreachable," while a real job still flows end to end—submitted through `api`'s published port, queued and processed via `redis`, and downloaded successfully.

`standalone` mode has no separate worker containers to isolate this way, same as [PDF worker isolation](#pdf-worker-isolation) above.

## FFmpeg hardening

ffmpeg's demuxers are built to auto-detect a file's actual structure from its content, not its extension—which is exactly the behavior a real-world SSRF/local-file-disclosure class of ffmpeg exploit relies on: a file merely *named* `clip.mp3` but structured as an HLS playlist or a `concat` script can make ffmpeg fetch an attacker-chosen `http(s)://` URL (probing internal services from the worker's network) or read an arbitrary local file the worker process can see, entirely through references embedded inside the file itself. Two flags close this off on every ffmpeg/ffprobe invocation this codebase makes—audio and video alike—both at upload-time validation and at actual conversion:

- **`-f <format>`** forces the exact expected demuxer (`mp3`, `wav`, `flac`, `ogg`, `mp4`, or `webm`) instead of leaving format selection to content sniffing. A file that doesn't actually parse as that format fails outright—ffmpeg never gets the chance to decide "this is actually an HLS playlist" in the first place. (`mp4`/`webm` aren't literally ffmpeg's own *demuxer* names on the input side—those are the combined `mov,mp4,m4a,3gp,3g2,mj2` and `matroska,webm`—but ffmpeg's format-name resolution accepts them as aliases into those on input too, confirmed directly against a real ffmpeg binary rather than assumed from the `-formats` listing.)
- **`-protocol_whitelist file`** additionally restricts every protocol ffmpeg is allowed to use—for the direct input and for anything a crafted container might reference internally—to plain local file I/O. No `http`, `https`, `rtmp`, `concat`, or anything else.

`-analyzeduration`/`-probesize` are also pinned to explicit values (rather than relying on ffmpeg's own version-dependent defaults) to bound how much of a file ffmpeg reads while determining stream parameters, and every upload is probed with `ffprobe` before conversion: audio is rejected unless it has exactly one audio stream whose codec is on a fixed per-container whitelist (an embedded cover-art picture is allowed through; any other video, subtitle, or data stream—or a second audio stream—is not) and a duration under 4 hours; video is rejected unless it has exactly one video stream and at most one audio stream, both on a fixed per-container whitelist, a 720p pixel ceiling, a 60-second duration cap, and no other stream of any kind (no cover-art-style exception for video).

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
