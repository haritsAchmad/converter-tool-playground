package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OCR is PDF -> DOCX's fallback for a page with no extractable text layer
// (a scanned/image-only page), and only for that: a page ledongthuc/pdf
// already extracted text from is never rendered or OCR'd, so an ordinary
// text PDF costs exactly what it did before this existed. It needs two
// external tools, both optional like every other engine here—poppler's
// pdftoppm to rasterize the one page, and Tesseract to read it—and is
// simply unavailable (PDF -> DOCX then behaves as it always did) when
// either is missing or none of the configured languages is installed.
//
// Tesseract never sees an uploaded byte directly. Its input is always a
// PNG this process had pdftoppm render moments earlier from a PDF that
// already passed validateSyntax's structural validation—the same
// renderer-as-boundary shape PDF -> image has—rather than an image
// pulled out of the PDF's own streams (pdfcpu can extract those, and a
// scan is usually exactly one image per page, but that would hand
// Leptonica's JPEG/JBIG2/CCITT decoders attacker-controlled bytes).

// defaultOCRLanguages is CONVERTBOX_OCR_LANGUAGES' default: Indonesian
// first, then English. Order matters to Tesseract—the first language's
// model is tried first—and languages that aren't installed are dropped
// at startup rather than failing every OCR job (see configureOCR).
const defaultOCRLanguages = "ind+eng"

// maxOCRLanguages bounds how many language models one OCR run loads:
// each one multiplies recognition time, and a list longer than this is
// almost certainly a misconfiguration rather than a real need.
const maxOCRLanguages = 4

// maxOCRPages bounds how many pages one job will OCR. A text page costs
// microseconds to extract; an OCR page costs a pdftoppm render plus a
// Tesseract run, on the order of a second or more each—so the existing
// maxPDFPages (300) bound is far too loose for this path, and the job
// deadline alone would let a 300-page scan burn a worker for its entire
// timeout before failing anyway.
const maxOCRPages = 50

// ocrRenderBoxPixels is the pixel box pdftoppm scales the page into
// (-scale-to): 3508 is A4's long side at 300 DPI, the resolution
// Tesseract's own documentation recommends. A box rather than a DPI
// (-r) on purpose: a PDF's MediaBox is attacker-controlled, and a fixed
// DPI against a page declared as, say, 200 inches wide would ask
// pdftoppm for a multi-gigapixel bitmap. The box caps the rendered
// image at ocrRenderBoxPixels squared (~12 MB as 8-bit gray) whatever
// the page claims its size is.
const ocrRenderBoxPixels = 3508

// maxOCRPageTextBytes bounds one page's recognized text. A dense A4
// page is a few kilobytes; this only exists so a pathological image
// can't make Tesseract emit unbounded output into memory. The total
// across pages is still bounded by maxPDFExtractedTextBytes.
const maxOCRPageTextBytes = 1 << 20

// ocrLanguagePattern matches a Tesseract language/script model name as
// its traineddata files are named: three lowercase letters, optionally
// followed by an underscore-separated variant (eng, ind, chi_sim,
// deu_latf). Deliberately closed: the value ends up in Tesseract's -l
// argument and, inside Tesseract, in a file path under tessdata.
var ocrLanguagePattern = regexp.MustCompile(`^[a-z]{3}(_[a-z]{2,8})?$`)

// pageOCR recognizes the text on one page (1-based) of the PDF at
// pdfPath. A function type so convertPDFToDocx's fallback logic—which
// pages are OCR'd, the page cap, what happens when OCR finds nothing—can
// be tested without either external tool installed.
type pageOCR func(ctx context.Context, pdfPath string, page int) (string, error)

// parseOCRLanguages splits a "+"-separated language spec (Tesseract's
// own -l syntax) into its validated, de-duplicated languages in order.
func parseOCRLanguages(spec string) ([]string, error) {
	var langs []string
	seen := map[string]bool{}
	for _, lang := range strings.Split(spec, "+") {
		lang = strings.TrimSpace(lang)
		if !ocrLanguagePattern.MatchString(lang) {
			return nil, fmt.Errorf("invalid OCR language %q (expected names like \"eng\" or \"ind\", joined with \"+\")", lang)
		}
		if !seen[lang] {
			seen[lang] = true
			langs = append(langs, lang)
		}
	}
	if len(langs) > maxOCRLanguages {
		return nil, fmt.Errorf("at most %d OCR languages are supported, got %d", maxOCRLanguages, len(langs))
	}
	return langs, nil
}

// configureOCR resolves which of the requested languages Tesseract
// actually has installed and stores them, in the requested order, as
// the -l value every OCR run uses. An unusable spec, a missing
// Tesseract, or no requested language being installed all leave
// c.ocrLangs empty, which is what makes ocrAvailable false—OCR is then
// simply not offered, the same "unavailable engines are not advertised"
// rule every other optional tool here follows, rather than each job
// failing inside Tesseract on a missing traineddata file.
func (c *converter) configureOCR(spec string) {
	c.ocrLangs = ""
	if c.tesseract == "" {
		return
	}
	requested, err := parseOCRLanguages(spec)
	if err != nil {
		return
	}
	installed := installedOCRLanguages(c.tesseract)
	var usable []string
	for _, lang := range requested {
		if installed[lang] {
			usable = append(usable, lang)
		}
	}
	c.ocrLangs = strings.Join(usable, "+")
}

// installedOCRLanguages asks Tesseract which language models it can
// find (--list-langs). Its first line is a human-readable header naming
// the tessdata directory; every following line is one model name, so
// lines are kept only if they look like one.
func installedOCRLanguages(tesseract string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tesseract, "--list-langs")
	cmd.Env = ocrEnvironment(tesseract)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); ocrLanguagePattern.MatchString(line) {
			installed[line] = true
		}
	}
	return installed
}

// ocrEnvironment is the whole environment a Tesseract process gets:
// PATH narrowed to its own directory (the same rule convertPDF applies
// to pdftoppm), TESSDATA_PREFIX passed through when the operator set it
// (the only way to point Tesseract at a non-default tessdata directory),
// and OMP_THREAD_LIMIT=1. Tesseract's LSTM engine otherwise spins up
// OpenMP threads on every core for one page, which makes concurrent
// jobs fight each other and, inside a CPU-limited container, is
// documented to run slower than a single thread.
func ocrEnvironment(tesseract string) []string {
	env := []string{"PATH=" + filepath.Dir(tesseract), "OMP_THREAD_LIMIT=1"}
	if prefix := os.Getenv("TESSDATA_PREFIX"); prefix != "" {
		env = append(env, "TESSDATA_PREFIX="+prefix)
	}
	return env
}

// ocrAvailable reports whether a page without a text layer can be OCR'd:
// both tools present and at least one configured language installed.
func (c *converter) ocrAvailable() bool {
	return c.tesseract != "" && c.pdftoppm != "" && c.ocrLangs != ""
}

// pageOCR returns the OCR function convertPDFToDocx should fall back
// to, or nil when OCR isn't available.
func (c *converter) pageOCR() pageOCR {
	if c.ocrOverride != nil {
		return c.ocrOverride
	}
	if !c.ocrAvailable() {
		return nil
	}
	return c.ocrPDFPage
}

// ocrPDFPage renders one page of the PDF to a grayscale PNG with
// pdftoppm and has Tesseract read it. The PNG lives next to the job's
// input (the job's own private directory) and is removed before
// returning, whatever happens. Both processes are bound to ctx, so a
// cancelled or timed-out job kills whichever one is running.
func (c *converter) ocrPDFPage(ctx context.Context, pdfPath string, page int) (string, error) {
	root := filepath.Join(filepath.Dir(pdfPath), fmt.Sprintf("ocr-page-%d", page))
	image := root + ".png"
	defer os.Remove(image)

	pageArg := strconv.Itoa(page)
	render := exec.CommandContext(ctx, c.pdftoppm,
		"-f", pageArg, "-l", pageArg, "-singlefile",
		"-scale-to", strconv.Itoa(ocrRenderBoxPixels), "-gray", "-png",
		pdfPath, root)
	render.Dir = filepath.Dir(pdfPath)
	render.Env = []string{"PATH=" + filepath.Dir(c.pdftoppm)}
	if output, err := render.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("rendering page %d for OCR failed: %w (%s)", page, err, strings.TrimSpace(string(output)))
	}

	// --dpi tells Tesseract what the box above corresponds to for an
	// ordinary A4/Letter page; without it Tesseract finds no usable
	// resolution in the PNG and guesses one, with a warning per page.
	recognize := exec.CommandContext(ctx, c.tesseract, image, "stdout", "-l", c.ocrLangs, "--dpi", "300")
	recognize.Dir = filepath.Dir(pdfPath)
	recognize.Env = ocrEnvironment(c.tesseract)
	stdout := cappedBuffer{limit: maxOCRPageTextBytes}
	stderr := cappedBuffer{limit: 2048}
	recognize.Stdout = &stdout
	recognize.Stderr = &stderr
	if err := recognize.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("OCR of page %d failed: %w (%s)", page, err, strings.TrimSpace(stderr.buf.String()))
	}
	if stdout.overflowed {
		return "", fmt.Errorf("OCR of page %d produced more text than the safety limit allows", page)
	}
	return cleanOCRText(stdout.buf.String()), nil
}

// cleanOCRText turns Tesseract's stdout into the same shape extracted
// text has: "\n"-separated lines with no trailing page separator.
// Tesseract ends every page with a form feed, which is not a character
// XML 1.0 allows and would otherwise reach the DOCX as U+FFFD.
func cleanOCRText(text string) string {
	text = strings.ReplaceAll(text, "\f", "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.TrimRight(text, " \t\n")
}

// cappedBuffer keeps at most limit bytes of what's written to it and
// records whether anything was dropped. It never returns a short write:
// doing so would make os/exec treat the pipe as broken and kill the
// process with an error unrelated to the real problem.
type cappedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room < len(p) {
		b.overflowed = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// errOCRPageLimit is returned for a PDF that has no text layer at all
// and more pages than one job will OCR.
func errOCRPageLimit(pages int) error {
	return fmt.Errorf("this PDF has no text layer and %d pages; OCR is limited to %d pages per file", pages, maxOCRPages)
}

// errNoPDFText is convertPDFToDocx's "nothing useful to put in the DOCX"
// error, worded for whichever situation actually applies so the person
// reading it knows whether OCR was tried.
func errNoPDFText(ocrTried bool, ocrErr error) error {
	switch {
	case ocrErr != nil:
		return fmt.Errorf("no extractable text found in this PDF, and OCR failed: %w", ocrErr)
	case ocrTried:
		return errors.New("no text found in this PDF, even with OCR (blank pages, or writing the OCR languages installed here can't read)")
	}
	return errors.New("no extractable text found in this PDF (image-only pages or an unsupported font encoding), and OCR is not available on this server")
}
