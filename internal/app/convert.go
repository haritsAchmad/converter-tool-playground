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
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
)

type converter struct{ magick, pdftoppm, libreoffice string }

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
}

var dataFormats = map[string]bool{"csv": true, "json": true, "xml": true, "yaml": true}
var imageFormats = map[string]bool{"png": true, "jpeg": true, "webp": true}
var officeFormats = map[string]bool{"docx": true, "xlsx": true, "pptx": true}

func newConverter() *converter {
	magick, _ := exec.LookPath("magick")
	pdftoppm, _ := exec.LookPath("pdftoppm")
	libreoffice, _ := exec.LookPath("libreoffice")
	if libreoffice == "" {
		libreoffice, _ = exec.LookPath("soffice")
	}
	return &converter{magick: magick, pdftoppm: pdftoppm, libreoffice: libreoffice}
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
	if (in == "markdown" || in == "html") && out == "pdf" {
		return c.libreoffice != ""
	}
	if imageFormats[in] && out == "pdf" {
		// Pure Go via pdfcpu (already a dependency for PDF structural
		// validation)—no external tool required, so always available.
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
	if in == "pdf" {
		return c.convertPDF(ctx, out, inPath, outPath)
	}
	if imageFormats[in] && out == "pdf" {
		return convertImageToPDF(inPath, outPath)
	}
	if imageFormats[in] {
		return c.convertImage(ctx, in, out, inPath, outPath)
	}
	if officeFormats[in] {
		return c.convertOffice(ctx, in, pdfMode, inPath, outPath)
	}
	if (in == "markdown" || in == "html") && out == "pdf" {
		return c.convertMarkupToPDF(ctx, in, inPath, outPath)
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
// just reached through a document instead of a filename. It's a
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
