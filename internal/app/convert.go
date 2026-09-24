package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
)

type converter struct{ magick, pdftoppm, libreoffice, ffmpeg, ffprobe string }

var formats = map[string]Format{
	"csv":      {"csv", "CSV", "Data", []string{".csv"}},
	"json":     {"json", "JSON", "Data", []string{".json"}},
	"xml":      {"xml", "XML", "Data", []string{".xml"}},
	"yaml":     {"yaml", "YAML", "Data", []string{".yaml", ".yml"}},
	"png":      {"png", "PNG", "Image", []string{".png"}},
	"jpeg":     {"jpeg", "JPEG / JPG", "Image", []string{".jpg", ".jpeg"}},
	"webp":     {"webp", "WebP", "Image", []string{".webp"}},
	"markdown": {"markdown", "Markdown", "Document", []string{".md", ".markdown"}},
	"html":     {"html", "HTML", "Document", []string{".html", ".htm"}},
	"pdf":      {"pdf", "PDF", "Document", []string{".pdf"}},
	"docx":     {"docx", "Word (DOCX)", "Office", []string{".docx"}},
	"xlsx":     {"xlsx", "Excel (XLSX)", "Office", []string{".xlsx"}},
	"pptx":     {"pptx", "PowerPoint (PPTX)", "Office", []string{".pptx"}},
	"odt":      {"odt", "OpenDocument Text (ODT)", "OpenDocument", []string{".odt"}},
	"ods":      {"ods", "OpenDocument Spreadsheet (ODS)", "OpenDocument", []string{".ods"}},
	"odp":      {"odp", "OpenDocument Presentation (ODP)", "OpenDocument", []string{".odp"}},
	"mp3":      {"mp3", "MP3", "Audio", []string{".mp3"}},
	"wav":      {"wav", "WAV", "Audio", []string{".wav"}},
	"flac":     {"flac", "FLAC", "Audio", []string{".flac"}},
	"ogg":      {"ogg", "Ogg (Vorbis/Opus)", "Audio", []string{".ogg"}},
	"mp4":      {"mp4", "MP4 (H.264)", "Video", []string{".mp4"}},
	"webm":     {"webm", "WebM (VP8/VP9)", "Video", []string{".webm"}},
	"svg":      {"svg", "SVG", "Image", []string{".svg"}},
}

var dataFormats = map[string]bool{"csv": true, "json": true, "xml": true, "yaml": true}
var imageFormats = map[string]bool{"png": true, "jpeg": true, "webp": true}
var officeFormats = map[string]bool{"docx": true, "xlsx": true, "pptx": true}

// audioFormats are converted via ffmpeg/ffprobe (see convertAudio and
// validateAudio). The format id doubles as the ffmpeg/ffprobe demuxer and
// muxer short name for each (verified: "mp3", "wav", "flac", and "ogg" are
// all real libavformat format names, not guessed), which is what lets
// convertAudio force -f <id> on both the input and output side instead of
// leaving format auto-detection to content sniffing.
var audioFormats = map[string]bool{"mp3": true, "wav": true, "flac": true, "ogg": true}

// videoFormats are converted via ffmpeg/ffprobe, the same engine and
// hardening as audioFormats (see convertVideo and validateVideo). "mp4"
// and "webm" are muxer names ffmpeg accepts verbatim for -f on output;
// there's no demuxer literally named either on the *input* side (the real
// demuxers are the combined "mov,mp4,m4a,3gp,3g2,mj2" and
// "matroska,webm"), but ffmpeg's format-name resolution accepts "mp4"/
// "webm" as aliases into those on input too—verified empirically against
// a real ffmpeg binary (not assumed from the -formats listing, which
// only shows the combined demuxer names and would suggest otherwise).
// Deliberately narrower in scope than audio: exactly one video stream, at
// most one audio stream, no subtitle/data stream of any kind (no
// attached-picture exception either—unlike audio, that's not an
// established convention for these containers), a 720p pixel ceiling, and
// a 60-second duration cap, keeping a job within reach of this project's
// existing job-timeout model without adding video-specific config.
var videoFormats = map[string]bool{"mp4": true, "webm": true}

// odfFormats are ODF's own package family (odt/ods/odp), deliberately kept
// separate from officeFormats: they go through the same convertOffice/
// convertViaLibreOffice pipeline (LibreOffice's own native format, so the
// same writer_pdf_Export/calc_pdf_Export/impress_pdf_Export filters
// apply), but pdfMode's fidelity controls were verified specifically
// against OOXML's filter registry entries, not ODF's, so pdfMode stays
// inapplicable here—resolvePDFMode's applicability check is
// officeFormats-only on purpose, and convertOffice is always called with
// pdfMode="" for this family (see converter.run).
var odfFormats = map[string]bool{"odt": true, "ods": true, "odp": true}

// svgOutputFormats are the only formats SVG input may be converted to:
// PNG and JPEG, both rasterized in pure Go via oksvg/rasterx (see
// convertSVG)—deliberately one-directional (no format converts TO svg,
// and svg never round-trips as svg->svg, unlike every other image pair
// in imageFormats). This is the "rasterization boundary" half of the
// roadmap's "SVG only after a dedicated sanitizer and rasterization
// boundary" requirement: an untrusted SVG is never handed to anything
// that treats it as a document to open (a browser, an SVG-aware image
// library with scripting support)—it is only ever rasterized into an
// inert bitmap by a renderer that structurally can't execute or fetch
// anything (verified directly against oksvg's source: its element
// dispatch table implements only path/shape/gradient/group elements
// plus a same-document-only <use>, and the package imports no
// networking or exec package anywhere). WebP isn't offered here the
// same way it isn't offered for PDF->image: there's no pure-Go WebP
// encoder in this project's dependencies (image conversion's own
// svg/webp pairs already shell out to ImageMagick instead), and adding
// an external-tool round trip just for this would undercut the point
// of a pure-Go rasterization boundary.
var svgOutputFormats = map[string]bool{"png": true, "jpeg": true}

// pdfTextExtractionFormats are the "Experimental PDF -> Office
// extraction" roadmap item's actual, deliberately narrow scope: PDF ->
// DOCX only, plain text only, one paragraph per line and a page break
// between each PDF page—explicitly NOT layout-preserving (no attempt
// at columns, tables, fonts, or image placement), matching the
// roadmap's own warning not to advertise this as layout-perfect. Text
// is pulled per page with github.com/ledongthuc/pdf's GetPlainText,
// chosen after confirming pdfcpu (already a dependency here) has no
// plain-text extraction API of its own—only raw content-stream/image/
// font/page extraction, which would need a hand-written PDF
// content-stream interpreter (font encoding, CMaps, text positioning)
// to turn into readable text, a much larger and easier-to-get-subtly-
// wrong undertaking than reusing a maintained, focused library for
// it. Verified before depending on it: pure Go, BSD-3-Clause, no
// net/net-http/os-exec import anywhere in the package (checked
// directly against its source, the same bar oksvg/rasterx were held
// to), no govulncheck advisories, and its own GetPlainText already
// recovers internally from a parse panic into a returned error rather
// than crashing the process—convertPDFToDocx adds its own outer
// recover too, since that guarantee doesn't necessarily extend to
// every call this codebase makes into the package (Open/NumPage/Page).
var pdfTextExtractionFormats = map[string]bool{"docx": true}

func newConverter() *converter {
	magick, _ := exec.LookPath("magick")
	pdftoppm, _ := exec.LookPath("pdftoppm")
	libreoffice, _ := exec.LookPath("libreoffice")
	if libreoffice == "" {
		libreoffice, _ = exec.LookPath("soffice")
	}
	ffmpeg, _ := exec.LookPath("ffmpeg")
	ffprobe, _ := exec.LookPath("ffprobe")
	return &converter{magick: magick, pdftoppm: pdftoppm, libreoffice: libreoffice, ffmpeg: ffmpeg, ffprobe: ffprobe}
}

func (c *converter) capabilities() []publicFormat {
	result := make([]publicFormat, 0, len(formats))
	for id, f := range formats {
		outs := []string{}
		for out := range formats {
			if c.supports(id, out) {
				outs = append(outs, out)
			}
		}
		if len(outs) > 0 {
			result = append(result, publicFormat{id, f.Label, f.Group, f.Extensions, outs})
		}
	}
	return result
}

func (c *converter) supports(in, out string) bool {
	if in == out {
		return false
	}
	if dataFormats[in] && dataFormats[out] {
		return true
	}
	if (in == "markdown" && out == "html") || (in == "html" && out == "markdown") {
		return true
	}
	if in == "pdf" && (out == "png" || out == "jpeg") {
		return c.pdftoppm != ""
	}
	if officeFormats[in] && out == "pdf" {
		return c.libreoffice != ""
	}
	if odfFormats[in] && out == "pdf" {
		return c.libreoffice != ""
	}
	if (in == "markdown" || in == "html") && out == "pdf" {
		return c.libreoffice != ""
	}
	if audioFormats[in] && audioFormats[out] {
		return c.ffmpeg != "" && c.ffprobe != ""
	}
	if videoFormats[in] && videoFormats[out] {
		return c.ffmpeg != "" && c.ffprobe != ""
	}
	if imageFormats[in] && out == "pdf" {
		// Pure Go via pdfcpu (already a dependency for PDF structural
		// validation)—no external tool required, so always available.
		return true
	}
	if in == "svg" && svgOutputFormats[out] {
		// Pure Go via oksvg/rasterx—no external tool required, so always
		// available, same as the imageFormats->pdf pair above.
		return true
	}
	if in == "pdf" && pdfTextExtractionFormats[out] {
		// Pure Go via github.com/ledongthuc/pdf plus a hand-written
		// minimal DOCX writer—no external tool required, so always
		// available. Experimental text-only extraction, not a
		// layout-preserving conversion—see pdfTextExtractionFormats.
		return true
	}
	if imageFormats[in] && imageFormats[out] {
		if in == "webp" || out == "webp" {
			return c.magick != ""
		}
		return true
	}
	return false
}

// outputExtension is the file extension a job should be stored/downloaded
// under for an in->out pair. Usually the target format's own registered
// extension, except PDF->image, which always produces a ZIP of per-page
// images (see convertPDF)—the target format token stays "png"/"jpeg" (the
// same value the API/UI already offer), but the bytes on disk are an
// archive, not a bare image, so the extension has to reflect that instead
// of formats[out].Extensions[0].
//
// This is the SINGLE source of truth for that extension—store.reload()
// also calls it (re-deriving the expected extension to validate the
// on-disk job.json sidecar, since a split API/worker deployment treats
// that shared file as untrusted-until-checked state). A second,
// independently written copy of this rule previously existed there and
// drifted the moment PDF->image stopped being a bare extension (temuan
// review P1: reload() rejected every PDF->PNG/JPEG job outright, and the
// worker silently dropped them from the queue without converting).
// Returns "" for an out that isn't a registered format at all, rather than
// panicking on formats[out].Extensions[0] against a zero-value Format—
// reload() treats that the same as "job cannot be trusted", the same
// safe-reject behavior its own prior format/ok, len(...) == 0 check had.
func outputExtension(in, out string) string {
	if in == "pdf" && imageFormats[out] {
		return ".zip"
	}
	format, ok := formats[out]
	if !ok || len(format.Extensions) == 0 {
		return ""
	}
	return format.Extensions[0]
}

// legacyOutputExtensions returns extension(s) a job.json sidecar for in->out
// might carry from BEFORE outputExtension's current answer for that pair,
// so store.reload() can still find an already-finished job's output file
// under its old name. The only pair whose convention has ever changed is
// PDF->image: it used to write a bare image directly and now always writes
// a ZIP of every page (see convertPDF)—a job that COMPLETED under the old
// rule has a real "output.png"/"output.jpg" on disk that will never be
// rewritten, so it must stay reachable via that name until it expires
// (temuan review P2: making reload() strictly ZIP-only for this pair,
// while fixing the P1 above, broke status/download for every PDF->image
// job that had already finished before that fix shipped). Returns nil for
// every other pair, which has only ever had one extension.
func legacyOutputExtensions(in, out string) []string {
	if in == "pdf" && imageFormats[out] {
		if format, ok := formats[out]; ok && len(format.Extensions) > 0 {
			return []string{format.Extensions[0]}
		}
	}
	return nil
}

func (c *converter) run(ctx context.Context, in, out, pdfMode, inPath, outPath string) error {
	if !c.supports(in, out) {
		return errors.New("conversion pair is not supported")
	}
	if dataFormats[in] {
		return convertData(in, out, inPath, outPath)
	}
	if in == "pdf" && pdfTextExtractionFormats[out] {
		return convertPDFToDocx(ctx, inPath, outPath)
	}
	if in == "pdf" {
		return c.convertPDF(ctx, out, inPath, outPath)
	}
	if imageFormats[in] && out == "pdf" {
		return convertImageToPDF(inPath, outPath)
	}
	if in == "svg" {
		return convertSVG(out, inPath, outPath)
	}
	if imageFormats[in] {
		return c.convertImage(ctx, in, out, inPath, outPath)
	}
	if officeFormats[in] {
		return c.convertOffice(ctx, in, pdfMode, inPath, outPath)
	}
	if odfFormats[in] && out == "pdf" {
		// pdfMode never applies to ODF (see odfFormats's doc comment), so
		// this always calls convertOffice with pdfMode="" regardless of
		// what the caller passed—resolvePDFMode already refuses to resolve
		// a non-blank pdfMode for this pair, so it can't be non-"" here in
		// practice, but this makes that invariant explicit rather than
		// relying on it silently.
		return c.convertOffice(ctx, in, "", inPath, outPath)
	}
	if (in == "markdown" || in == "html") && out == "pdf" {
		return c.convertMarkupToPDF(ctx, in, inPath, outPath)
	}
	if audioFormats[in] && audioFormats[out] {
		return c.convertAudio(ctx, in, out, inPath, outPath)
	}
	if videoFormats[in] && videoFormats[out] {
		return c.convertVideo(ctx, in, out, inPath, outPath)
	}
	return convertDocument(in, out, inPath, outPath)
}

// PDFModeStandard (the default, used whenever a job doesn't specify
// pdfMode) leaves LibreOffice's export filter untouched: whatever page
// setup, fonts, and image fidelity the source document's own styles
// already specify come through as-is, same as opening File > Export As
// PDF with no options changed.
//
// PDFModeOptimized opts into filter data (see officePDFFilterOptions) that
// actively reshapes the export: every Calc sheet is forced onto exactly
// one PDF page regardless of its own print setup (SinglePageSheets), the
// 14 standard PDF fonts are embedded so viewers can't silently substitute
// a different font than what LibreOffice rendered with (EmbedStandardFonts),
// and embedded images are downsampled to a web-friendly 150 DPI
// (ReduceImageResolution/MaxImageResolution) for a smaller file. This is a
// deliberate, opt-in trade—layout may shift from what the source document
// would print as—never the default, per the roadmap's "keep a fast
// standard mode... rather than silently rewriting every document's
// layout."
const (
	PDFModeStandard  = "standard"
	PDFModeOptimized = "optimized"
)

// resolvePDFMode validates the optional pdfMode form field against the
// resolved in/out pair. Blank stays blank for every pair except
// Office->PDF, where it normalizes to PDFModeStandard so job status
// reporting is explicit about which mode actually applied rather than
// leaving it ambiguous between "not applicable" and "defaulted".
func resolvePDFMode(in, out, raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	applicable := officeFormats[in] && out == "pdf"
	if raw != "" && !applicable {
		return "", errors.New("pdfMode is only applicable when converting an Office document to PDF")
	}
	if !applicable {
		return "", nil
	}
	if raw == "" {
		raw = PDFModeStandard
	}
	if raw != PDFModeStandard && raw != PDFModeOptimized {
		return "", fmt.Errorf("pdfMode must be %q or %q", PDFModeStandard, PDFModeOptimized)
	}
	return raw, nil
}

// officePDFFilterName maps a source Office format to the LibreOffice PDF
// export filter that understands its format-specific options (verified
// against LibreOffice's own filter registry: filter/source/config/
// fragments/filters/{writer,calc,impress}_pdf_Export.xcu).
var officePDFFilterName = map[string]string{
	"docx": "writer_pdf_Export",
	"xlsx": "calc_pdf_Export",
	"pptx": "impress_pdf_Export",
}

// officePDFFilterOptions returns the --convert-to filter-data JSON object
// (without the enclosing pdf:<filter>: prefix) for pdfMode, or "" for
// PDFModeStandard/unknown values, meaning "pass no filter data at all"
// (LibreOffice's own defaults). SinglePageSheets is Calc-only per its own
// documented behavior ("ignores each sheet's paper size, print ranges and
// shown/hidden status and puts every sheet on exactly one page"); the
// font/image options are common to all three *_pdf_Export filters.
func officePDFFilterOptions(in, pdfMode string) string {
	if pdfMode != PDFModeOptimized {
		return ""
	}
	opts := `"EmbedStandardFonts":{"type":"boolean","value":"true"},` +
		`"ReduceImageResolution":{"type":"boolean","value":"true"},` +
		`"MaxImageResolution":{"type":"long","value":"150"}`
	if in == "xlsx" {
		opts = `"SinglePageSheets":{"type":"boolean","value":"true"},` + opts
	}
	return "{" + opts + "}"
}

// convertOffice runs LibreOffice with a fresh per-job profile. The uploaded
// file is staged with its verified extension because job storage deliberately
// uses an opaque input.bin name and LibreOffice's filter detection is more
// deterministic when the OOXML extension is present.
func (c *converter) convertOffice(ctx context.Context, in, pdfMode, inPath, outPath string) error {
	if c.libreoffice == "" {
		return errors.New("Office conversion is not available")
	}
	workDir, err := os.MkdirTemp(filepath.Dir(inPath), ".libreoffice-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	if err := os.Chmod(workDir, 0700); err != nil {
		return err
	}
	staged := filepath.Join(workDir, "input."+in)
	if err := copyPrivateFile(inPath, staged); err != nil {
		return err
	}
	convertTo := "pdf"
	if opts := officePDFFilterOptions(in, pdfMode); opts != "" {
		convertTo = "pdf:" + officePDFFilterName[in] + ":" + opts
	}
	return c.convertViaLibreOffice(ctx, workDir, staged, convertTo, outPath)
}

// convertMarkupToPDF renders Markdown or HTML to PDF via the same headless
// LibreOffice engine and per-job profile isolation convertOffice uses for
// Office documents. Markdown is rendered to HTML first with goldmark in its
// default *safe* mode—raw HTML embedded in the Markdown source is dropped
// from the output rather than passed through—rather than handed to
// LibreOffice directly, since a bundled headless LibreOffice cannot be
// relied on to have a Markdown import filter at all, let alone one that
// renders CommonMark correctly; this reuses the exact rendering
// convertDocument already does for markdown->html.
//
// Unlike the pure-Go HTML<->Markdown text conversion this codebase already
// had, this path actually RENDERS the document with a real layout engine
// that resolves references, so it goes through validateHTMLForPDF first:
// <script>/<iframe>/inline-event-handler content is rejected, and every
// resource reference (an <img src>, a stylesheet <link>, a CSS url(...),
// ...) is accepted only as an inline data: URI—including a same-directory
// relative path or an absolute filesystem path, not just an external
// http(s) URL, since LibreOffice resolves either against the document's
// real on-disk location and a job's per-job working directory is not a
// sandbox. That's the same SSRF/local-file-read-shaped risk already
// flagged for FFmpeg's network-capable input protocols on the roadmap,
// just reached through a document instead of a filename. The accepted
// data: URIs are themselves a closed allowlist of raster image formats,
// cross-checked against their actual decoded bytes and bounded to the
// same declared-dimension ceiling an ordinary image upload already gets,
// not "any data: URI"—a data:text/css payload, say, decodes to a
// stylesheet LibreOffice parses exactly like a <style> block, so its own
// content still needs the same reference check applied recursively, and
// a data:image/svg+xml payload can embed a <script> of its own. It's a
// default-deny allowlist for resource references, parsed and checked
// against the same decoded attribute values LibreOffice's own parser would
// act on (not pattern-matched against the raw, possibly HTML-entity-
// encoded bytes)—see validateHTMLForPDF—but still not a full sanitizer;
// real defense in depth still wants the LibreOffice worker denied outbound
// network access at the OS/container level, which remains follow-up
// hardening (see ROADMAP.md), same as the egress-denied worker isolation
// already noted for Office->PDF.
func (c *converter) convertMarkupToPDF(ctx context.Context, in, inPath, outPath string) error {
	if c.libreoffice == "" {
		return errors.New("PDF rendering is not available")
	}
	b, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	html := b
	if in == "markdown" {
		var buf bytes.Buffer
		if err := goldmark.Convert(b, &buf); err != nil {
			return err
		}
		html = []byte("<!doctype html>\n<html><head><meta charset=\"utf-8\"></head><body>\n" + buf.String() + "</body></html>\n")
	}
	if err := validateHTMLForPDF(html); err != nil {
		return err
	}
	workDir, err := os.MkdirTemp(filepath.Dir(inPath), ".libreoffice-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	if err := os.Chmod(workDir, 0700); err != nil {
		return err
	}
	staged := filepath.Join(workDir, "input.html")
	if err := os.WriteFile(staged, html, 0600); err != nil {
		return err
	}
	// No format-specific --convert-to filter name (unlike convertOffice's
	// officePDFFilterName): pdfMode never applies to this pair (see
	// resolvePDFMode), and a dedicated HTML PDF-export filter name/option
	// set could not be found in LibreOffice's own filter documentation to
	// verify rather than guess, so this deliberately lets LibreOffice
	// auto-select its default export filter for whatever it imported.
	return c.convertViaLibreOffice(ctx, workDir, staged, "pdf", outPath)
}

// convertViaLibreOffice runs headless LibreOffice's --convert-to against
// staged (already placed under workDir, named with the extension
// LibreOffice needs to detect the right import filter), verifies a
// non-empty PDF came out, and moves it to outPath. Shared by convertOffice
// and convertMarkupToPDF, which differ only in how the input gets staged
// and which --convert-to filter string applies.
func (c *converter) convertViaLibreOffice(ctx context.Context, workDir, staged, convertTo, outPath string) error {
	profile := filepath.Join(workDir, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		return err
	}
	profileURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(profile)}).String()
	cmd := exec.CommandContext(ctx, c.libreoffice,
		"--headless", "--invisible", "--nologo", "--nodefault", "--nolockcheck", "--norestore",
		"-env:UserInstallation="+profileURL,
		"--convert-to", convertTo, "--outdir", workDir, staged,
	)
	cmd.Dir = workDir
	cmd.Env = officeEnvironment(c.libreoffice, workDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("LibreOffice conversion failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	generated := strings.TrimSuffix(staged, filepath.Ext(staged)) + ".pdf"
	info, err := os.Stat(generated)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("LibreOffice conversion produced no PDF (%s)", strings.TrimSpace(string(output)))
	}
	if err := os.Rename(generated, outPath); err != nil {
		return err
	}
	return os.Chmod(outPath, 0600)
}

func officeEnvironment(executable, workDir string) []string {
	path := filepath.Dir(executable)
	if runtime.GOOS == "windows" {
		// The native Windows launcher may rely on system DLL/helper lookup.
		// Arguments remain fixed and are never passed through a shell.
		path += string(os.PathListSeparator) + os.Getenv("PATH")
	} else {
		// Alpine's /usr/bin/libreoffice is a shell wrapper that calls ls, sed,
		// grep, and uname from /bin before it reaches the real binary.
		path += ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return []string{"HOME=" + workDir, "PATH=" + path, "LANG=C.UTF-8"}
}

func copyPrivateFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// convertPDF renders every page of the PDF (up to maxPDFPages—also enforced
// at upload validation time, but repeated here via -l as defense in depth)
// via poppler's pdftoppm: a mature, actively CVE-patched renderer, run
// out-of-process with a job deadline so a hostile PDF can burn at most that
// much wall time before it's killed. pdftoppm writes one file per page
// (root-1.<ext>, root-2.<ext>, ...); those are zipped into a single
// page-N.<ext> per entry output and then removed, keeping this a plain
// 1-job-1-output-file conversion like every other format here even though
// the source may have many pages.
func (c *converter) convertPDF(ctx context.Context, out, inPath, outPath string) error {
	if c.pdftoppm == "" {
		return errors.New("PDF rendering is not available")
	}
	ext := "png"
	args := []string{"-r", "150", "-l", strconv.Itoa(maxPDFPages)}
	if out == "jpeg" {
		args = append(args, "-jpeg")
		ext = "jpg"
	} else {
		args = append(args, "-png")
	}
	root := strings.TrimSuffix(outPath, filepath.Ext(outPath))
	args = append(args, inPath, root)
	cmd := exec.CommandContext(ctx, c.pdftoppm, args...)
	cmd.Dir = filepath.Dir(inPath)
	cmd.Env = []string{"PATH=" + filepath.Dir(c.pdftoppm)}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("PDF rendering failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	pages, err := renderedPDFPages(root, ext)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		return errors.New("PDF rendering produced no output (empty or encrypted PDF?)")
	}
	if err := zipRenderedPages(ctx, outPath, pages, ext); err != nil {
		return err
	}
	for _, p := range pages {
		_ = os.Remove(p.path)
	}
	return nil
}

type renderedPDFPage struct {
	number int
	path   string
}

// renderedPDFPages finds pdftoppm's per-page output (root-<n>.<ext>,
// zero-padded to a width that depends on the page count, so a fixed-width
// pattern can't be assumed) and returns them sorted by actual page number
// rather than filename, since e.g. "root-10.png" sorts before "root-2.png"
// lexicographically.
func renderedPDFPages(root, ext string) ([]renderedPDFPage, error) {
	matches, err := filepath.Glob(root + "-*." + ext)
	if err != nil {
		return nil, err
	}
	pageNumRe := regexp.MustCompile(`-(\d+)\.` + regexp.QuoteMeta(ext) + `$`)
	pages := make([]renderedPDFPage, 0, len(matches))
	for _, m := range matches {
		sub := pageNumRe.FindStringSubmatch(m)
		if sub == nil {
			continue
		}
		n, err := strconv.Atoi(sub[1])
		if err != nil {
			continue
		}
		pages = append(pages, renderedPDFPage{number: n, path: m})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].number < pages[j].number })
	return pages, nil
}

// zipRenderedPages packages every rendered page into outPath. It checks ctx
// before each page and, via copyIntoZip's contextReader, between chunks
// within a page's own copy too—not just relying on pdftoppm's own ctx-bound
// process exiting. Without the per-page check, cancelling or timing out a
// job only stopped pdftoppm while the zip loop kept copying however many
// hundreds of already-rendered pages remained; without the per-chunk check,
// a cancellation arriving mid-copy of whichever page was in flight (e.g. the
// last, or the only, page) still let that copyIntoZip call run to
// completion and report success (temuan review P2, both rounds). A last
// ctx check after zw.Close() covers a cancellation landing during
// finalization (flushing the central directory) after every page already
// copied cleanly.
func zipRenderedPages(ctx context.Context, outPath string, pages []renderedPDFPage, ext string) (err error) {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	zw := zip.NewWriter(f)
	for _, p := range pages {
		if err = ctx.Err(); err != nil {
			_ = zw.Close()
			return err
		}
		if err = copyIntoZip(ctx, zw, p, ext); err != nil {
			_ = zw.Close()
			return err
		}
	}
	if err = zw.Close(); err != nil {
		return err
	}
	return ctx.Err()
}

func copyIntoZip(ctx context.Context, zw *zip.Writer, p renderedPDFPage, ext string) error {
	src, err := os.Open(p.path)
	if err != nil {
		return err
	}
	defer src.Close()
	w, err := zw.Create(fmt.Sprintf("page-%d.%s", p.number, ext))
	if err != nil {
		return err
	}
	return copyWithContext(ctx, w, src)
}

// copyWithContext is io.Copy with a per-chunk ctx check via contextReader,
// factored out of copyIntoZip so the cancelled-mid-copy case can be tested
// deterministically against a controllable reader instead of a real file
// and real timing.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, contextReader{ctx: ctx, r: src})
	return err
}

// contextReader aborts a Read once ctx is done, so io.Copy checks
// cancellation between chunks instead of only before or after a whole
// page's copy—a cancelled job stops mid-copy of whichever page is in
// flight rather than finishing it regardless.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr contextReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// maxPDFExtractedTextBytes bounds the total plain text convertPDFToDocx
// will accumulate across every page before writing it out. Unlike
// convertPDF's image rendering, where output size scales predictably
// with page count and a fixed DPI, extracted text size has no such
// natural relationship to the source PDF's byte size—a pathological
// content stream could in principle decode to far more text bytes than
// the file itself—so this caps it directly rather than only relying on
// the existing maxPDFPages page-count bound.
const maxPDFExtractedTextBytes = 20 << 20

// convertPDFToDocx is the "Experimental PDF -> Office extraction"
// roadmap item, scoped to exactly what pdfTextExtractionFormats' doc
// comment describes: plain text only, one DOCX paragraph per extracted
// line, a page break between each source PDF page, nothing else. A
// page ledongthuc/pdf can't extract text from (an unusual font
// encoding, or a scanned/image-only page with no text layer at all)
// contributes an empty page rather than failing the whole job—but if
// literally nothing came back non-blank across every page, the job
// fails outright with a clear reason instead of silently producing a
// DOCX with nothing useful in it.
func convertPDFToDocx(ctx context.Context, inPath, outPath string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PDF text extraction panicked: %v", r)
		}
	}()
	f, r, err := pdf.Open(inPath)
	if err != nil {
		return fmt.Errorf("could not open PDF for text extraction: %w", err)
	}
	defer f.Close()
	numPages := r.NumPage()
	if numPages <= 0 {
		return errors.New("PDF has no pages to extract text from")
	}
	if numPages > maxPDFPages {
		// Already enforced at upload validation (validateSyntax's PDF
		// case); repeated here as defense in depth against a job that
		// somehow reaches conversion without having gone through it.
		numPages = maxPDFPages
	}
	pages := make([]string, numPages)
	total := 0
	nonBlank := false
	for i := 1; i <= numPages; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, extractErr := r.Page(i).GetPlainText(nil)
		if extractErr != nil {
			continue // best-effort: leave this one page blank, don't fail the job
		}
		total += len(text)
		if total > maxPDFExtractedTextBytes {
			return errors.New("extracted text exceeds the safety limit")
		}
		if strings.TrimSpace(text) != "" {
			nonBlank = true
		}
		pages[i-1] = text
	}
	if !nonBlank {
		return errors.New("no extractable text found in this PDF (image-only pages or an unsupported font encoding)")
	}
	return writeDocx(outPath, pages)
}

// docxPageSizeTwips is the US Letter page size (8.5in x 11in, in
// twentieths of a point—the unit WordprocessingML's w:pgSz uses), the
// single most common default in a real Word-authored document, per
// ECMA-376. writeDocx's output carries no layout information of its own
// (see pdfTextExtractionFormats' doc comment), so this is only here
// because most DOCX consumers expect a w:sectPr with a page size to be
// present at all, not because it means anything about the source PDF's
// own page size.
const docxPageSizeTwips = `<w:pgSz w:w="12240" w:h="15840"/>`

// writeDocx hand-builds a minimal, valid WordprocessingML (.docx)
// package—this codebase otherwise only ever validates/reads OOXML
// (validateOOXML), never writes it, so there's no existing writer to
// reuse. The three parts below ([Content_Types].xml, _rels/.rels,
// word/document.xml) are the documented minimum ECMA-376 requires for a
// package to be recognized as a WordprocessingML document at all. Each
// input page becomes one or more paragraphs (one per line, split on
// "\n"; a blank line becomes an empty paragraph rather than being
// dropped, preserving the source's blank-line spacing) followed by an
// explicit page break before the next page's content, so page
// boundaries from the source PDF survive into the output even though
// nothing else about its layout does.
func writeDocx(outPath string, pages []string) error {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	if err := writeZipEntry(zw, "[Content_Types].xml", []byte(xml.Header+`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)); err != nil {
		_ = zw.Close()
		return err
	}
	if err := writeZipEntry(zw, "_rels/.rels", []byte(xml.Header+`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`)); err != nil {
		_ = zw.Close()
		return err
	}

	var body bytes.Buffer
	for i, page := range pages {
		if i > 0 {
			body.WriteString(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
		}
		for _, line := range strings.Split(page, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				body.WriteString(`<w:p/>`)
				continue
			}
			body.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
			_ = xml.EscapeText(&body, []byte(line))
			body.WriteString(`</w:t></w:r></w:p>`)
		}
	}
	document := xml.Header +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() +
		`<w:sectPr>` + docxPageSizeTwips + `</w:sectPr></w:body></w:document>`
	if err := writeZipEntry(zw, "word/document.xml", []byte(document)); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return nil
}

func writeZipEntry(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func convertDocument(in, out, inPath, outPath string) error {
	b, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	var result []byte
	if in == "markdown" && out == "html" {
		var buf bytes.Buffer
		if err := goldmark.Convert(b, &buf); err != nil {
			return err
		}
		result = []byte("<!doctype html>\n<html><head><meta charset=\"utf-8\"></head><body>\n" + buf.String() + "</body></html>\n")
	} else {
		converted, err := md.ConvertString(string(b))
		if err != nil {
			return err
		}
		result = []byte(converted)
	}
	return os.WriteFile(outPath, result, 0600)
}

func (c *converter) convertImage(ctx context.Context, in, out, inPath, outPath string) error {
	if in == "webp" || out == "webp" {
		cmd := exec.CommandContext(ctx, c.magick, inPath, "-auto-orient", "-strip", outPath)
		cmd.Dir = filepath.Dir(inPath)
		cmd.Env = []string{"PATH=" + filepath.Dir(c.magick)}
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("image conversion failed: %w (%s)", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	f, err := os.Open(inPath)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer outFile.Close()
	if out == "png" {
		return png.Encode(outFile, img)
	}
	return jpeg.Encode(outFile, img, &jpeg.Options{Quality: 90})
}

// pdfImportDPI is the pixel-to-point conversion rate used when sizing a
// generated PDF page to its source image, matching the DPI already used
// elsewhere in this project for PDF<->image rendering (convertPDF) so a
// round trip through both conversions doesn't shift apparent print size.
const pdfImportDPI = 150.0

// convertImageToPDF wraps a single PNG/JPEG/WebP image as a one-page PDF
// via pdfcpu (already a dependency for PDF structural validation, and pure
// Go—no external tool needed, unlike every other conversion pair that
// touches PDF or WebP in this file). The page is sized to the image's own
// pixel dimensions (converted at pdfImportDPI) and the image is stretched
// to fill it exactly, rather than pdfcpu's default of a fixed A4 page with
// the image inset at half scale, since a converter's implicit job is "make
// this a PDF", not "print this small on a letterhead".
func convertImageToPDF(inPath, outPath string) error {
	f, err := os.Open(inPath)
	if err != nil {
		return err
	}
	cfg, _, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("could not read image dimensions: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return errors.New("image has invalid dimensions")
	}
	imp := api.DefaultImportConfig()
	imp.UserDim = true
	imp.Pos = types.Full
	imp.PageDim = &types.Dim{
		Width:  float64(cfg.Width) * 72 / pdfImportDPI,
		Height: float64(cfg.Height) * 72 / pdfImportDPI,
	}
	return api.ImportImagesFile([]string{inPath}, outPath, imp, model.NewDefaultConfiguration())
}

// defaultSVGCanvasSize is the raster canvas side length (in pixels) used
// when an SVG's root element declares no width, height, or viewBox at
// all (oksvg's own attribute parser does strip common unit suffixes
// like "px" before calling strconv.ParseFloat—verified directly against
// its source—so a width="200px"/height="200px" pair with no viewBox
// parses fine; it's a fully dimensionless <svg> that leaves ViewBox.W/H
// at zero). Rather than reject a real-world SVG over this, convertSVG
// falls back to a fixed square canvas and skips SetTarget (which would
// otherwise divide by a zero ViewBox dimension), rendering in the SVG's
// own coordinate space onto that canvas—a lower-fidelity but still safe
// result.
const defaultSVGCanvasSize = 512

// convertSVG is the "rasterization boundary" half of the roadmap's "SVG
// only after a dedicated sanitizer and rasterization boundary"
// requirement (the sanitizer half is validateSVG, applied at upload
// time). It renders the SVG with oksvg/rasterx, a pure-Go rasterizer
// with no scripting engine and no networking or filesystem access of
// its own (verified directly against its source: the element dispatch
// table in its draw.go implements only svg/g/line/rect/circle/ellipse/
// polyline/polygon/path/title/desc/defs/style/linearGradient/
// radialGradient/use—no script, no image, no foreignObject, no SMIL
// animation—and neither oksvg nor rasterx imports net/http, net, or
// os/exec anywhere in either package), so even an SVG that somehow
// slipped past validateSVG's own explicit denylist can only fail to
// render the parts this parser doesn't understand, never execute or
// fetch anything through it. This holds independently of validateSVG,
// by construction of the library, not because validateSVG is trusted to
// have already caught everything.
func convertSVG(out, inPath, outPath string) error {
	icon, err := oksvg.ReadIcon(inPath)
	if err != nil {
		return fmt.Errorf("could not parse SVG: %w", err)
	}
	w, h := int(icon.ViewBox.W), int(icon.ViewBox.H)
	if w <= 0 || h <= 0 {
		w, h = defaultSVGCanvasSize, defaultSVGCanvasSize
	} else {
		icon.SetTarget(0, 0, float64(w), float64(h))
	}
	// Same decompression-bomb-shaped ceiling as an ordinary PNG/JPEG
	// upload's declared dimensions (maxImageDecodedPixels)—a hostile SVG
	// can declare an arbitrarily large viewBox just as freely as a PNG
	// header can lie about its dimensions, and the raster canvas below is
	// allocated eagerly at this size regardless of how little the SVG
	// actually draws into it.
	if int64(w)*int64(h) > maxImageDecodedPixels {
		return errors.New("SVG canvas dimensions exceed the safety limit")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if out == "jpeg" {
		// JPEG has no alpha channel; an SVG's default (unpainted) canvas
		// is transparent, which would otherwise encode as black.
		draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	}
	scanner := rasterx.NewScannerGV(w, h, img, img.Bounds())
	raster := rasterx.NewDasher(w, h, scanner)
	icon.Draw(raster, 1.0)

	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer outFile.Close()
	if out == "png" {
		return png.Encode(outFile, img)
	}
	return jpeg.Encode(outFile, img, &jpeg.Options{Quality: 90})
}

// audioOutputEncoder maps each accepted output container to the specific
// codec (and a fixed quality/bitrate) ffmpeg encodes it with, rather than
// leaving container->default-codec selection to ffmpeg itself, which can
// vary by build. libmp3lame and libvorbis are both confirmed present in
// Alpine's ffmpeg package (ffmpeg-libavcodec depends on libmp3lame.so.0
// and libvorbis.so.0/libvorbisenc.so.2, the same package this project's
// Dockerfile installs), not assumed bundled.
var audioOutputEncoder = map[string][]string{
	"mp3":  {"-c:a", "libmp3lame", "-b:a", "192k"},
	"wav":  {"-c:a", "pcm_s16le"},
	"flac": {"-c:a", "flac"},
	"ogg":  {"-c:a", "libvorbis", "-q:a", "5"},
}

// ffmpegHardeningArgs are the hardening flags shared by every ffmpeg/
// ffprobe invocation that touches a user-supplied audio or video file,
// whether probing it (validateAudio/validateVideo) or actually
// transcoding it (convertAudio/convertVideo). Only options both tools
// actually recognize belong here—"-nostdin" is deliberately NOT among
// them (temuan review P1): it's an ffmpeg-CLI-only option (defined
// alongside ffmpeg.c's own option table, not ffprobe's), and ffprobe
// rejects it outright, failing every single upload's validation before it
// ever got to read the file. It's added separately, only in
// convertAudio's/convertVideo's own args, where it belongs.
//
//   - "-protocol_whitelist file" keeps every protocol other than plain
//     local file I/O out of reach, for both the direct input and anything
//     a crafted container might reference internally. Verified against
//     real-world SSRF/LFI writeups for ffmpeg's HLS/concat demuxers, which
//     is exactly the class of attack this closes off: without it, a file
//     merely named "*.mp3" but structured as an HLS playlist or concat
//     script could make ffmpeg fetch an internal http(s) URL or read an
//     arbitrary local file the worker process can see. This one IS shared
//     with ffprobe correctly: protocol whitelisting is a libavformat
//     (AVOption) concern both tools link against, not an ffmpeg-CLI one.
//   - "-f <format>" (appended by each caller, not here, since it differs
//     per call) forces the exact demuxer rather than leaving format
//     selection to ffmpeg's own content-based auto-detection, so a file
//     that doesn't actually parse as that format fails outright instead
//     of ffmpeg falling back to guessing what it might be--including
//     guessing "this is actually an HLS playlist", the same class of
//     confusion the protocol whitelist alone doesn't fully close (a
//     concat/HLS demuxer's own nested references are still resolved
//     through whatever protocols are whitelisted).
//   - "-analyzeduration"/"-probesize" bound how much of the file ffmppeg
//     will read while determining stream parameters, pinned to explicit
//     values rather than relying on ffmpeg's own version-dependent
//     defaults (these have changed across ffmpeg releases)--also a
//     libavformat concern both tools share, not ffmpeg-CLI-only.
//   - "-v error" keeps each tool's own diagnostic chatter out of stdout,
//     so validateAudio's/validateVideo's JSON parse only ever sees
//     ffprobe's actual output.
var ffmpegHardeningArgs = []string{"-v", "error", "-protocol_whitelist", "file", "-analyzeduration", "5000000", "-probesize", "5000000"}

// convertAudio transcodes in (one of audioFormats) to out via ffmpeg,
// mapping only the input's single validated audio stream (-map 0:a:0) and
// explicitly dropping video/subtitle/data streams (-vn -sn -dn)--
// including the attached-picture "video" stream validateAudio allows
// through for cover art, which has no equivalent in most of these output
// containers and would otherwise carry through unpredictably depending on
// the target format.
func (c *converter) convertAudio(ctx context.Context, in, out, inPath, outPath string) error {
	if c.ffmpeg == "" {
		return errors.New("audio conversion is not available")
	}
	// -nostdin is ffmpeg-CLI-only (see ffmpegHardeningArgs's doc comment for
	// why it isn't in the shared list)--added here, not in
	// ffmpegHardeningArgs, since this is the one call site that's actually
	// running ffmpeg, not ffprobe. It stops ffmpeg from ever waiting on
	// interactive input.
	args := append([]string{"-nostdin"}, ffmpegHardeningArgs...)
	args = append(args, "-f", in, "-i", inPath, "-map", "0:a:0", "-vn", "-sn", "-dn")
	args = append(args, audioOutputEncoder[out]...)
	args = append(args, "-f", out, outPath)
	cmd := exec.CommandContext(ctx, c.ffmpeg, args...)
	cmd.Dir = filepath.Dir(inPath)
	cmd.Env = []string{"PATH=" + filepath.Dir(c.ffmpeg)}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("audio conversion failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// videoOutputEncoder maps each accepted video output container to the
// specific codecs (and a fast preset/deadline, since a job stays within
// this project's existing job-timeout model rather than a
// video-specific one—see videoFormats's doc comment) ffmpeg encodes it
// with. libx264/libvpx-vp9/libopus availability was confirmed directly
// against the exact `ffmpeg-libavcodec` package this project's own
// `alpine:3.22` Dockerfile installs (its dependency list includes
// libx264.so, libvpx.so, and libopus.so), not assumed. AAC uses ffmpeg's
// own built-in encoder, no external library needed.
var videoOutputEncoder = map[string][]string{
	"mp4":  {"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac"},
	"webm": {"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-c:a", "libopus"},
}

// convertVideo transcodes in (one of videoFormats) to out via ffmpeg,
// reusing the same protocol-whitelist/format-forcing/probe-size hardening
// as convertAudio (ffmpegHardeningArgs), plus the same -nostdin caveat
// (see convertAudio's own comment). Maps the input's single validated video
// stream and its at-most-one validated audio stream—0:a:0? rather than
// 0:a:0, so a silent (video-only) input, valid per validateVideoStreams,
// doesn't make ffmpeg fail looking for an audio stream that was never
// there—and explicitly drops subtitle/data streams (-sn -dn), matching
// validateVideoStreams's own reject-list of anything else.
func (c *converter) convertVideo(ctx context.Context, in, out, inPath, outPath string) error {
	if c.ffmpeg == "" {
		return errors.New("video conversion is not available")
	}
	args := append([]string{"-nostdin"}, ffmpegHardeningArgs...)
	args = append(args, "-f", in, "-i", inPath, "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn")
	args = append(args, videoOutputEncoder[out]...)
	args = append(args, "-f", out, outPath)
	cmd := exec.CommandContext(ctx, c.ffmpeg, args...)
	cmd.Dir = filepath.Dir(inPath)
	cmd.Env = []string{"PATH=" + filepath.Dir(c.ffmpeg)}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("video conversion failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func convertData(in, out, inPath, outPath string) error {
	b, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	value, err := decodeData(in, b)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", in, err)
	}
	result, err := encodeData(out, value)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, result, 0600)
}

func decodeData(format string, b []byte) (any, error) {
	var v any
	switch format {
	case "json":
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		if d.Decode(&struct{}{}) != io.EOF {
			return nil, errors.New("multiple JSON values")
		}
		return v, nil
	case "yaml":
		if err := yaml.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		return normalizeYAML(v), nil
	case "csv":
		r := csv.NewReader(bytes.NewReader(b))
		r.ReuseRecord = false
		rows, err := r.ReadAll()
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return []any{}, nil
		}
		head := rows[0]
		seen := map[string]bool{}
		for _, h := range head {
			if strings.TrimSpace(h) == "" || seen[h] {
				return nil, errors.New("CSV headers must be non-empty and unique")
			}
			seen[h] = true
		}
		items := make([]any, 0, len(rows)-1)
		for _, row := range rows[1:] {
			if len(row) != len(head) {
				return nil, errors.New("CSV row has different column count")
			}
			m := map[string]any{}
			for i, h := range head {
				m[h] = row[i]
			}
			items = append(items, m)
		}
		return items, nil
	case "xml":
		return decodeXML(b)
	}
	return nil, errors.New("unknown data format")
}

func encodeData(format string, v any) ([]byte, error) {
	switch format {
	case "json":
		return json.MarshalIndent(v, "", "  ")
	case "yaml":
		return yaml.Marshal(v)
	case "csv":
		return encodeCSV(v)
	case "xml":
		return encodeXML(v)
	}
	return nil, errors.New("unknown output format")
}

func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalizeYAML(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeYAML(x[i])
		}
		return x
	default:
		return x
	}
}

func encodeCSV(v any) ([]byte, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("CSV output requires an array of flat objects")
	}
	if len(items) == 0 {
		return []byte{}, nil
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		return nil, errors.New("CSV rows must be objects")
	}
	head := make([]string, 0, len(first))
	for k := range first {
		head = append(head, k)
	}
	sortStrings(head)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	rawHead := make([]string, len(head))
	for i, h := range head {
		rawHead[i] = defuseCSVFormula(h)
	}
	_ = w.Write(rawHead)
	for _, item := range items {
		rowMap, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("CSV rows must be objects")
		}
		row := make([]string, len(head))
		for i, k := range head {
			val, exists := rowMap[k]
			if !exists {
				return nil, errors.New("CSV rows must share the same fields")
			}
			switch val.(type) {
			case map[string]any, []any:
				return nil, errors.New("CSV does not support nested values")
			}
			row[i] = defuseCSVFormula(scalarString(val))
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// defuseCSVFormula guards against CSV/formula injection: a cell that opens
// with =, +, -, @, tab, or CR is interpreted as a formula by Excel, Sheets,
// and LibreOffice when the file is later opened, which can run commands or
// exfiltrate data (OWASP "CSV Injection"). Prefixing it with a quote keeps
// the value intact as inert text instead.
func defuseCSVFormula(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
func scalarString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprint(v)
}
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []xmlNode  `xml:",any"`
}

func decodeXML(b []byte) (any, error) {
	var n xmlNode
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	if err := d.Decode(&n); err != nil {
		return nil, err
	}
	return map[string]any{n.XMLName.Local: xmlNodeValue(n)}, nil
}
func xmlNodeValue(n xmlNode) any {
	if len(n.Children) == 0 && len(n.Attrs) == 0 {
		return strings.TrimSpace(n.Text)
	}
	m := map[string]any{}
	for _, a := range n.Attrs {
		m["@"+a.Name.Local] = a.Value
	}
	for _, c := range n.Children {
		v := xmlNodeValue(c)
		if old, ok := m[c.XMLName.Local]; ok {
			if list, ok := old.([]any); ok {
				m[c.XMLName.Local] = append(list, v)
			} else {
				m[c.XMLName.Local] = []any{old, v}
			}
		} else {
			m[c.XMLName.Local] = v
		}
	}
	if t := strings.TrimSpace(n.Text); t != "" {
		m["#text"] = t
	}
	return m
}
func encodeXML(v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "root"}}); err != nil {
		return nil, err
	}
	if err := writeXML(enc, "item", v); err != nil {
		return nil, err
	}
	if err := enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "root"}}); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
func writeXML(enc *xml.Encoder, name string, v any) error {
	if !validXMLName(name) {
		name = "field"
	}
	start := xml.StartElement{Name: xml.Name{Local: name}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			if !strings.HasPrefix(k, "@") {
				keys = append(keys, k)
			}
		}
		sortStrings(keys)
		for _, k := range keys {
			if err := writeXML(enc, k, x[k]); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range x {
			if err := writeXML(enc, "item", item); err != nil {
				return err
			}
		}
	default:
		if err := enc.EncodeToken(xml.CharData([]byte(scalarString(x)))); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}
func validXMLName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r == '-' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
