package app

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"
)

var blockedExt = map[string]bool{".exe": true, ".dll": true, ".com": true, ".bat": true, ".cmd": true, ".ps1": true, ".sh": true, ".php": true, ".js": true, ".jar": true, ".msi": true, ".scr": true, ".vbs": true, ".py": true, ".pl": true}

func validateUpload(path, original string) (string, error) {
	ext := strings.ToLower(filepath.Ext(original))
	if hasBlockedExtension(original) {
		return "", errors.New("executable and script files are not accepted")
	}
	var candidate string
	for id, f := range formats {
		for _, allowed := range f.Extensions {
			if ext == allowed {
				candidate = id
			}
		}
	}
	if candidate == "" {
		return "", errors.New("file extension is not on the input whitelist")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := f.Read(head)
	head = head[:n]
	if looksExecutable(head) {
		return "", errors.New("file signature looks executable")
	}
	if err := rejectActiveContent(path); err != nil {
		return "", err
	}
	mime := http.DetectContentType(head)
	switch candidate {
	case "png":
		if !bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
			return "", errors.New("extension and PNG signature do not match")
		}
	case "jpeg":
		if len(head) < 3 || head[0] != 0xff || head[1] != 0xd8 || head[2] != 0xff {
			return "", errors.New("extension and JPEG signature do not match")
		}
	case "webp":
		if len(head) < 12 || string(head[:4]) != "RIFF" || string(head[8:12]) != "WEBP" {
			return "", errors.New("extension and WebP signature do not match")
		}
	case "pdf":
		if !bytes.HasPrefix(head, []byte("%PDF-")) {
			return "", errors.New("extension and PDF signature do not match")
		}
	case "docx", "xlsx", "pptx":
		if len(head) < 4 || !bytes.Equal(head[:4], []byte{'P', 'K', 0x03, 0x04}) {
			return "", errors.New("extension and OOXML ZIP signature do not match")
		}
	default:
		if bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(head) {
			return "", errors.New("text input must be valid UTF-8 without NUL bytes")
		}
	}
	if imageFormats[candidate] && candidate != "webp" {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(head))
		if err != nil {
			return "", errors.New("image header is invalid")
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxImageDecodedPixels {
			return "", errors.New("image dimensions exceed the safety limit")
		}
	}
	if err := validateSyntax(candidate, path); err != nil {
		return "", err
	}
	if !mimeAllowed(candidate, mime) {
		return "", errors.New("detected MIME type does not match the file format")
	}
	return candidate, nil
}

func hasBlockedExtension(name string) bool {
	name = strings.ToLower(filepath.Base(name))
	for ext := range blockedExt {
		if strings.Contains(name, ext+".") || strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func rejectActiveContent(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lower := bytes.ToLower(b)
	for _, marker := range [][]byte{[]byte("<?php"), []byte("<?=")} {
		if bytes.Contains(lower, marker) {
			return errors.New("active script content is not accepted")
		}
	}
	return nil
}

func validateSyntax(format, path string) error {
	if officeFormats[format] {
		return validateOOXML(format, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	switch format {
	case "json":
		var v any
		if json.Unmarshal(b, &v) != nil {
			return errors.New("invalid JSON syntax")
		}
	case "yaml":
		var v any
		if yaml.Unmarshal(b, &v) != nil {
			return errors.New("invalid YAML syntax")
		}
	case "csv", "xml":
		_, err := decodeData(format, b)
		if err != nil {
			return err
		}
	case "pdf":
		// Structural validation with a second, independent parser (pdfcpu,
		// pure Go) before the file ever reaches the native pdftoppm
		// renderer: a PDF crafted to exploit one specific parser's bug is
		// much less likely to also cleanly validate against a different one.
		if err := api.Validate(bytes.NewReader(b), model.NewDefaultConfiguration()); err != nil {
			return fmt.Errorf("invalid PDF structure: %w", err)
		}
		// Bounded up front, before rendering: convertPDF renders every page
		// into its own file and zips them, so an unbounded page count is an
		// unbounded amount of disk and pdftoppm wall time, not just a bigger
		// single image.
		count, err := api.PageCount(bytes.NewReader(b), model.NewDefaultConfiguration())
		if err != nil {
			return fmt.Errorf("could not determine PDF page count: %w", err)
		}
		if count > maxPDFPages {
			return fmt.Errorf("PDF has %d pages, exceeding the %d page rendering limit", count, maxPDFPages)
		}
	}
	return nil
}

const (
	maxOOXMLEntries          = 10000
	maxOOXMLUncompressedSize = 200 << 20
	// Declared width*height ceiling for PNG/JPEG input, checked against the
	// header BEFORE the full pixel buffer is ever allocated (image.Decode in
	// convertImage decodes unconditionally otherwise). 100 megapixels covers
	// any legitimate photo or scan for this service while keeping a hostile
	// file's worst-case decoded buffer in the hundreds-of-MB range instead of
	// unbounded — a small PNG/JPEG can freely lie about its dimensions in the
	// header (a classic "decompression bomb"), and the upload size limit
	// alone doesn't constrain that.
	maxImageDecodedPixels = 100_000_000
	// Page count ceiling for PDF input, checked here (before conversion)
	// and again passed to pdftoppm's -l flag as defense in depth. 300 pages
	// comfortably covers a thesis or report while bounding convertPDF's
	// worst case to 300 rendered files zipped into one output.
	maxPDFPages = 300
)

// dangerousHTMLTags are rejected outright regardless of attributes:
// known active-content elements, plus <base>, which can redirect every
// *relative* URL in the rest of the document to an attacker-chosen origin
// or filesystem path.
var dangerousHTMLTags = map[string]bool{"script": true, "iframe": true, "object": true, "embed": true, "applet": true, "base": true}

// resourceAttrs are the attributes LibreOffice's HTML import can actually
// fetch or read from while rendering: image/media/stylesheet references,
// plus the less common ones (poster, background, formaction/action, an
// <object>'s data) a hand-built or generated document could still use.
// href is deliberately excluded here and handled separately per-element,
// since an <a href> is never fetched—it just becomes a clickable
// annotation in the exported PDF.
var resourceAttrs = map[string]bool{"src": true, "srcset": true, "poster": true, "background": true, "formaction": true, "action": true, "data": true}

// cssURLPattern extracts the argument of a CSS url(...) function, used
// both on a parsed style="" attribute value and directly on a raw <style>
// block's text content.
var cssURLPattern = regexp.MustCompile(`(?i)url\(\s*["']?([^"')]*)["']?\s*\)`)

// validateHTMLForPDF gates convertMarkupToPDF's input (original HTML
// uploads, and goldmark-rendered HTML from a Markdown upload) right before
// it's handed to LibreOffice: that conversion actually renders the
// document with a real layout engine that resolves references, unlike the
// pure-Go HTML<->Markdown text transform this codebase already had, which
// never fetches or executes anything.
//
// This parses the document with golang.org/x/net/html rather than
// pattern-matching the raw bytes: an attribute value like
// src="&#104;ttp://127.0.0.1/probe.png" is an HTML entity-encoded "http:"
// that a real parser (LibreOffice's included) decodes back into a live
// URL before acting on it, so any check written against the undecoded
// text can be trivially bypassed the same way (temuan review P1). Walking
// the parsed tree and inspecting each attribute's decoded value closes
// that gap by construction—it checks the same value the renderer will.
//
// It's a default-deny allowlist, not a denylist of known-bad schemes: a
// resource reference (src/srcset/poster/background/formaction/action/an
// <object>'s data, a <link>'s href, or a CSS url(...) in a style
// attribute or a <style> block) is accepted only as an inline data: URI;
// everything else is rejected, including a bare relative or absolute
// filesystem path. An earlier version of this check allowed relative
// paths on the theory that the staged HTML has no sibling files to
// resolve them against, which missed that "relative" is resolved by
// LibreOffice against the *document's own location* on disk, and ".."
// segments (or an outright absolute path) can walk out of that per-job
// directory into a sibling job's files or anywhere else the worker
// process can read (temuan review P1, second finding)—there being no
// legitimate use for a local file reference at all (a job is always
// exactly one uploaded file, never a document-plus-assets bundle) makes
// rejecting every non-data: reference outright both safer and simpler
// than trying to canonicalize and allowlist a path. <script> and friends
// are rejected outright (dangerousHTMLTags), inline event-handler
// attributes (onclick=, onload=, ...) and javascript: URIs are rejected
// wherever they appear (including in an otherwise-allowed <a href>), and
// an <a href> to anything else is left alone, mirroring validateOOXML's
// own allowance for hyperlink relationships.
func validateHTMLForPDF(htmlBytes []byte) error {
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return fmt.Errorf("invalid HTML: %w", err)
	}
	var walk func(*html.Node) error
	walk = func(n *html.Node) error {
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if dangerousHTMLTags[tag] {
				return fmt.Errorf("HTML contains a <%s> element, which is not accepted for PDF rendering", tag)
			}
			for _, a := range n.Attr {
				name := strings.ToLower(a.Key)
				if strings.HasPrefix(name, "on") {
					return fmt.Errorf("HTML contains an inline event-handler attribute (%s), which is not accepted for PDF rendering", name)
				}
				if strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), "javascript:") {
					return errors.New("HTML contains a javascript: URI, which is not accepted for PDF rendering")
				}
				if name == "style" {
					if err := validateCSSForPDF(a.Val); err != nil {
						return err
					}
					continue
				}
				isHref := name == "href"
				if isHref && tag == "a" {
					continue // a hyperlink is never fetched; see doc comment.
				}
				if (resourceAttrs[name] || isHref) && !isSafeResourceRef(a.Val) {
					return fmt.Errorf("HTML contains a %s reference to something other than an inline data: URI, which is not accepted for PDF rendering", name)
				}
			}
		}
		if n.Type == html.ElementNode && strings.ToLower(n.Data) == "style" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					if err := validateCSSForPDF(c.Data); err != nil {
						return err
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(doc)
}

// isSafeResourceRef reports whether a resource reference is empty (no
// reference at all) or an inline data: URI carrying one of a closed set of
// raster image formats (see isSafeDataURI)—the only kind validateHTMLForPDF
// accepts. See its doc comment for why every other form, including a
// same-directory relative path, is rejected.
func isSafeResourceRef(val string) bool {
	val = strings.TrimSpace(val)
	return val == "" || isSafeDataURI(val)
}

// dataURIImageSignatures is the closed set of raster image formats
// accepted for an inline data: URI resource reference. Each is checked
// against the actual decoded bytes' own magic signature, not just the
// data: URI's self-declared media-type parameter, which is attacker-
// controlled and proves nothing on its own—the declared type is only used
// to pick which signature (and image.DecodeConfig format) applies, never
// trusted on its own to decide accept/reject.
//
// Deliberately excludes text/css and image/svg+xml (and everything else):
// both can themselves carry further resource references—a nested CSS
// url(...), an SVG <image>/<script>—that this validator has no visibility
// into once they're inside a data: payload it doesn't recursively inspect.
// A data:text/css;base64,... stylesheet used to pass an earlier,
// any-data:-URI-is-fine version of this check outright, while its decoded
// content—parsed by LibreOffice as an ordinary stylesheet exactly like a
// <style> block—could still carry an external url() reference this
// validator never looked at (temuan review P1, round 3). WebP is excluded
// too: this codebase has no Go-native WebP decoder (image conversion
// shells out to ImageMagick instead), so its dimensions can't be bounded
// the same way the other three are below—the same reason validateUpload's
// own decompression-bomb check already skips WebP today.
var dataURIImageSignatures = []struct {
	mime string
	sig  func([]byte) bool
}{
	{"image/png", func(b []byte) bool { return bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) }},
	{"image/jpeg", func(b []byte) bool { return len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff }},
	{"image/gif", func(b []byte) bool { return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a")) }},
}

// isSafeDataURI parses a data: URI (RFC 2397: "data:[<mediatype>][;base64],<data>")
// and accepts it only when the media type is one of dataURIImageSignatures,
// the payload is base64-encoded (a non-base64, percent-encoded text
// payload is rejected outright rather than decoded and re-scanned—none of
// the allowed binary image formats is sensibly represented that way), the
// decoded bytes actually match that format's signature, and the decoded
// image's declared dimensions stay within maxImageDecodedPixels—the exact
// same decompression-bomb guard validateUpload already applies to an
// ordinary PNG/JPEG upload, reused here since LibreOffice decodes this
// data the same way while rendering the PDF.
func isSafeDataURI(val string) bool {
	if !strings.HasPrefix(strings.ToLower(val), "data:") {
		return false
	}
	meta, payload, found := strings.Cut(val[len("data:"):], ",")
	if !found {
		return false
	}
	metaLower := strings.ToLower(meta)
	if !strings.HasSuffix(metaLower, ";base64") {
		return false
	}
	mime := strings.TrimSuffix(metaLower, ";base64")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(payload); err != nil {
			return false
		}
	}
	for _, candidate := range dataURIImageSignatures {
		if mime != candidate.mime || !candidate.sig(decoded) {
			continue
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
		if err != nil {
			return false
		}
		return cfg.Width > 0 && cfg.Height > 0 && int64(cfg.Width)*int64(cfg.Height) <= maxImageDecodedPixels
	}
	return false
}

// validateCSSForPDF applies the same data:-only policy to every CSS
// url(...) reference in css, whether that's a parsed style="" attribute
// value or a raw <style> block's text content. HTML entity references are
// not interpreted inside a <style> element's text per the HTML5 raw-text
// element rules (Go's html.Parse follows this: a <style>/<script>'s child
// text node is the literal, undecoded source), so checking the raw text
// directly is correct there, not a gap the way it would be for an
// attribute value.
func validateCSSForPDF(css string) error {
	for _, m := range cssURLPattern.FindAllStringSubmatch(css, -1) {
		if !isSafeResourceRef(m[1]) {
			return errors.New("HTML contains a CSS url() reference to something other than an inline data: URI, which is not accepted for PDF rendering")
		}
	}
	return nil
}

func validateOOXML(format, path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return errors.New("invalid OOXML ZIP structure")
	}
	defer zr.Close()
	if len(zr.File) == 0 || len(zr.File) > maxOOXMLEntries {
		return errors.New("OOXML package has an unsafe number of entries")
	}
	requiredRoot := map[string]string{"docx": "word/document.xml", "xlsx": "xl/workbook.xml", "pptx": "ppt/presentation.xml"}[format]
	foundTypes, foundRoot := false, false
	var total uint64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		clean := filepath.ToSlash(filepath.Clean(name))
		if name == "" || strings.HasPrefix(name, "/") || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, "../") {
			return errors.New("OOXML package contains an unsafe path")
		}
		total += f.UncompressedSize64
		zeroSizeMismatch := f.CompressedSize64 == 0 && f.UncompressedSize64 != 0
		if total > maxOOXMLUncompressedSize || zeroSizeMismatch || (f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > 200) {
			return errors.New("OOXML package exceeds decompression safety limits")
		}
		lower := strings.ToLower(clean)
		if strings.Contains(lower, "vba") || strings.Contains(lower, "/activex/") || strings.Contains(lower, "/embeddings/") {
			return errors.New("OOXML macros and embedded objects are not accepted")
		}
		if strings.HasSuffix(lower, ".rels") {
			if err := validateOOXMLRelationships(f); err != nil {
				return err
			}
		}
		if clean == "[Content_Types].xml" {
			foundTypes = true
		}
		if clean == requiredRoot {
			foundRoot = true
		}
	}
	if !foundTypes || !foundRoot {
		return errors.New("OOXML package is missing required document parts")
	}
	return nil
}

func validateOOXMLRelationships(f *zip.File) error {
	if f.UncompressedSize64 > 1<<20 {
		return errors.New("OOXML relationship part is too large")
	}
	r, err := f.Open()
	if err != nil {
		return errors.New("invalid OOXML relationship part")
	}
	defer r.Close()
	var relationships struct {
		Items []struct {
			Type       string `xml:"Type,attr"`
			TargetMode string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&relationships); err != nil {
		return errors.New("invalid OOXML relationship XML")
	}
	for _, rel := range relationships.Items {
		if strings.EqualFold(rel.TargetMode, "External") && !strings.HasSuffix(strings.ToLower(rel.Type), "/hyperlink") {
			return errors.New("OOXML external resources are not accepted")
		}
	}
	return nil
}
func looksExecutable(b []byte) bool {
	return bytes.HasPrefix(b, []byte("MZ")) || bytes.HasPrefix(b, []byte{0x7f, 'E', 'L', 'F'}) || bytes.HasPrefix(b, []byte("#!")) || bytes.HasPrefix(b, []byte{0xca, 0xfe, 0xba, 0xbe})
}
func mimeAllowed(format, mime string) bool {
	if imageFormats[format] {
		switch format {
		case "png":
			return mime == "image/png"
		case "jpeg":
			return mime == "image/jpeg"
		case "webp":
			return mime == "image/webp" || mime == "application/octet-stream"
		}
	}
	if format == "pdf" {
		return mime == "application/pdf"
	}
	if officeFormats[format] {
		return mime == "application/zip" || mime == "application/octet-stream" ||
			strings.HasPrefix(mime, "application/vnd.openxmlformats-officedocument.")
	}
	return strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/xml" || mime == "application/octet-stream"
}
