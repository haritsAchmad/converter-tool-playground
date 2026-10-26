# Credits

converter-tool-playground dibangun di atas karya orang lain. File ini mencatat library,
program, layanan, dan data pihak lain yang dipakai, beserta pembuat dan
lisensinya. Kode proyek ini sendiri berlisensi MIT (lihat [LICENSE](LICENSE)).

Setiap kali menambah library, program, data, model, suara, atau gambar
dari pihak lain, tambahkan juga ke file ini di PR yang sama.

Terakhir diperiksa: 26 Oktober 2026.

## Library Go (ikut di dalam binary)

Daftar dari `go list -deps ./...` (tanpa dependensi khusus tes); lisensi
dibaca dari file LICENSE di module cache. Teks lisensi lengkap ada di repo
masing-masing. Toolchain: [Go](https://go.dev) (BSD-3-Clause).

| Modul | Versi | Lisensi |
| --- | --- | --- |
| [github.com/JohannesKaufmann/dom](https://github.com/JohannesKaufmann/dom) | v0.2.0 | MIT |
| [github.com/JohannesKaufmann/html-to-markdown/v2](https://github.com/JohannesKaufmann/html-to-markdown) | v2.4.0 | MIT |
| [github.com/beorn7/perks](https://github.com/beorn7/perks) | v1.0.1 | MIT |
| [github.com/cespare/xxhash/v2](https://github.com/cespare/xxhash) | v2.3.0 | MIT |
| [github.com/clipperhouse/uax29/v2](https://github.com/clipperhouse/uax29) | v2.7.0 | MIT |
| [github.com/felixge/httpsnoop](https://github.com/felixge/httpsnoop) | v1.1.0 | MIT |
| [github.com/go-logr/logr](https://github.com/go-logr/logr) | v1.4.4 | Apache-2.0 |
| [github.com/go-logr/stdr](https://github.com/go-logr/stdr) | v1.2.2 | Apache-2.0 |
| [github.com/google/uuid](https://github.com/google/uuid) | v1.6.0 | BSD-3-Clause |
| [github.com/hhrutter/tiff](https://github.com/hhrutter/tiff) | v1.0.6 | BSD-3-Clause |
| [github.com/ledongthuc/pdf](https://github.com/ledongthuc/pdf) | v0.0.0-20260907135840-6c8c28e0e8a0 | BSD-3-Clause |
| [github.com/mattn/go-runewidth](https://github.com/mattn/go-runewidth) | v0.0.27 | MIT |
| [github.com/munnerz/goautoneg](https://github.com/munnerz/goautoneg) | v0.0.0-20191010083416-a7dc8b61c822 | BSD-3-Clause |
| [github.com/pdfcpu/pdfcpu](https://github.com/pdfcpu/pdfcpu) | v0.15.0 | Apache-2.0 |
| [github.com/prometheus/client_golang](https://github.com/prometheus/client_golang) | v1.24.1 | Apache-2.0 |
| [github.com/prometheus/client_model](https://github.com/prometheus/client_model) | v0.6.2 | Apache-2.0 |
| [github.com/prometheus/common](https://github.com/prometheus/common) | v0.70.1 | Apache-2.0 |
| [github.com/redis/go-redis/v9](https://github.com/redis/go-redis) | v9.22.0 | BSD-2-Clause |
| [github.com/srwiley/oksvg](https://github.com/srwiley/oksvg) | v0.0.0-20221011165216-be6e8873101c | BSD-3-Clause |
| [github.com/srwiley/rasterx](https://github.com/srwiley/rasterx) | v0.0.0-20220730225603-2ab79fcdd4ef | BSD-3-Clause |
| [github.com/yuin/goldmark](https://github.com/yuin/goldmark) | v1.7.13 | MIT |
| [go.opentelemetry.io/auto/sdk](https://pkg.go.dev/go.opentelemetry.io/auto/sdk) | v1.2.1 | Apache-2.0 |
| [go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) | v0.70.0 | Apache-2.0 |
| [go.opentelemetry.io/otel](https://pkg.go.dev/go.opentelemetry.io/otel) | v1.45.0 | Apache-2.0 |
| [go.opentelemetry.io/otel/metric](https://pkg.go.dev/go.opentelemetry.io/otel/metric) | v1.45.0 | Apache-2.0 |
| [go.opentelemetry.io/otel/trace](https://pkg.go.dev/go.opentelemetry.io/otel/trace) | v1.45.0 | Apache-2.0 |
| [go.uber.org/atomic](https://pkg.go.dev/go.uber.org/atomic) | v1.11.0 | MIT |
| [go.yaml.in/yaml/v3](https://pkg.go.dev/go.yaml.in/yaml/v3) | v3.0.5 | MIT dan Apache-2.0 |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) | v0.54.0 | BSD-3-Clause |
| [golang.org/x/image](https://pkg.go.dev/golang.org/x/image) | v0.44.0 | BSD-3-Clause |
| [golang.org/x/net](https://pkg.go.dev/golang.org/x/net) | v0.57.0 | BSD-3-Clause |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys) | v0.47.0 | BSD-3-Clause |
| [golang.org/x/text](https://pkg.go.dev/golang.org/x/text) | v0.40.0 | BSD-3-Clause |
| [google.golang.org/protobuf](https://pkg.go.dev/google.golang.org/protobuf) | v1.36.11 | BSD-3-Clause |
| [gopkg.in/yaml.v3](https://pkg.go.dev/gopkg.in/yaml.v3) | v3.0.1 | MIT dan Apache-2.0 |

## Program di dalam image Docker (paket Alpine 3.22)

| Program | Lisensi | Dipakai untuk |
| --- | --- | --- |
| [LibreOffice](https://www.libreoffice.org/) writer/calc/impress | MPL-2.0 | Konversi dokumen Office |
| [FFmpeg](https://ffmpeg.org/) / ffprobe | LGPL-2.1+ / GPL (sesuai build Alpine) | Konversi audio/video |
| [ImageMagick](https://imagemagick.org/) | ImageMagick License (Apache-2.0-style) | Konversi gambar |
| [Poppler](https://poppler.freedesktop.org/) `pdftoppm` | GPL-2.0/3.0 | PDF hasil scan ke gambar |
| [Tesseract OCR](https://github.com/tesseract-ocr/tesseract) + data eng/ind | Apache-2.0 | OCR |
| Font Liberation, DejaVu, Noto | SIL OFL 1.1 (Liberation, Noto); DejaVu: Bitstream Vera License | Render dokumen |

Image dasar: [Alpine Linux](https://alpinelinux.org/) dan `golang:1.26-alpine`
(build). Layanan pendamping di `compose.yaml`: [Redis](https://redis.io/)
8 (RSALv2/SSPLv1/AGPLv3), [Caddy](https://caddyserver.com/) 2 (Apache-2.0).
Program dipanggil sebagai proses terpisah; kodenya tidak disalin ke repo ini.

Ada yang kurang atau salah? Buka issue atau PR.
