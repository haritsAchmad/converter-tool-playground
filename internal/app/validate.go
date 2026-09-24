package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	case "odt", "ods", "odp":
		if len(head) < 4 || !bytes.Equal(head[:4], []byte{'P', 'K', 0x03, 0x04}) {
			return "", errors.New("extension and ODF ZIP signature do not match")
		}
		if !isODFMimetypeEntry(head, odfMimeType[candidate]) {
			return "", errors.New("ODF mimetype entry is missing or does not match the declared format")
		}
	case "wav":
		if len(head) < 12 || string(head[:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
			return "", errors.New("extension and WAV signature do not match")
		}
	case "flac":
		if !bytes.HasPrefix(head, []byte("fLaC")) {
			return "", errors.New("extension and FLAC signature do not match")
		}
	case "ogg":
		if !bytes.HasPrefix(head, []byte("OggS")) {
			return "", errors.New("extension and Ogg signature do not match")
		}
	case "mp3":
		if !isMP3Signature(head) {
			return "", errors.New("extension and MP3 signature do not match")
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
	if odfFormats[format] {
		return validateODF(format, path)
	}
	if audioFormats[format] {
		return validateAudio(format, path)
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
		// ReadAndValidate (rather than the plain Validate this used to
		// call) does the same validation pass but also hands back the
		// parsed *model.Context, which the page-count bound and the
		// content-disarm check below both need--avoiding a second and
		// third full re-parse of the same bytes.
		ctx, err := api.ReadAndValidate(bytes.NewReader(b), model.NewDefaultConfiguration())
		if err != nil {
			return fmt.Errorf("invalid PDF structure: %w", err)
		}
		// Bounded up front, before rendering: convertPDF renders every page
		// into its own file and zips them, so an unbounded page count is an
		// unbounded amount of disk and pdftoppm wall time, not just a bigger
		// single image.
		if ctx.PageCount > maxPDFPages {
			return fmt.Errorf("PDF has %d pages, exceeding the %d page rendering limit", ctx.PageCount, maxPDFPages)
		}
		if err := validatePDFActiveContent(ctx); err != nil {
			return err
		}
	}
	return nil
}

const (
	// Shared ZIP-package safety bounds for both document package families
	// this codebase accepts (OOXML: docx/xlsx/pptx, and ODF: odt/ods/odp)—
	// see validateOOXML and validateODF, which apply them identically.
	maxZIPPackageEntries          = 10000
	maxZIPPackageUncompressedSize = 200 << 20
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
	// Duration ceiling for audio input, checked against ffprobe's own
	// reported format duration before conversion. 4 hours comfortably
	// covers an audiobook or a long recording while bounding convertAudio's
	// worst-case transcode time, which the job timeout alone only catches
	// after a worker has already committed to running it.
	maxAudioDurationSeconds = 4 * 60 * 60
	// Wall-clock bound on the ffprobe call validateAudio makes at upload
	// time, separate from (and much shorter than) the job timeout that
	// bounds the actual conversion later—probing is supposed to be cheap
	// relative to transcoding, and a file that makes ffprobe itself take
	// this long is treated as suspect rather than waited out.
	audioProbeTimeout = 10 * time.Second
)

// audioCodecWhitelist is the closed set of audio codecs accepted inside
// each container audioFormats supports, keyed by ffprobe's own
// codec_name. libmp3lame/libvorbis availability (needed to re-encode into
// mp3/ogg) was confirmed against Alpine's ffmpeg package rather than
// assumed—see audioOutputEncoder in convert.go. wav is restricted to the
// common PCM variants rather than every codec WAV's format tag can
// technically carry; ogg deliberately excludes theora (Ogg can carry
// video) and anything else a container built for arbitrary codecs might
// smuggle in.
var audioCodecWhitelist = map[string]map[string]bool{
	"mp3":  {"mp3": true},
	"wav":  {"pcm_s16le": true, "pcm_s24le": true, "pcm_s32le": true, "pcm_u8": true, "pcm_f32le": true, "pcm_f64le": true},
	"flac": {"flac": true},
	"ogg":  {"vorbis": true, "opus": true, "flac": true},
}

// ffprobeOutput is the slice of ffprobe's own `-of json -show_streams
// -show_format` output validateAudio actually needs; unrecognized fields
// are ignored by encoding/json, so this doesn't have to mirror ffprobe's
// full schema.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}
type ffprobeStream struct {
	CodecName   string         `json:"codec_name"`
	CodecType   string         `json:"codec_type"`
	Disposition map[string]int `json:"disposition"`
}
type ffprobeFormat struct {
	Duration string `json:"duration"`
}

// isMP3Signature reports whether head starts with either an ID3v2 tag
// ("ID3", the common case for a real-world MP3 with metadata) or a raw
// MPEG audio frame sync (11 set bits: 0xFF followed by a byte whose top 3
// bits are also set). MP3 has no single universal magic number the way
// FLAC ("fLaC") or Ogg ("OggS") do—verified against Go's own net/http
// sniffer (sniff.go), which recognizes only the ID3-tagged form as
// "audio/mpeg" and falls back to a generic binary type for the bare
// frame-sync form, which is why mimeAllowed accepts both audio/mpeg and
// application/octet-stream for mp3.
func isMP3Signature(head []byte) bool {
	if bytes.HasPrefix(head, []byte("ID3")) {
		return true
	}
	return len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0
}

// validateAudio probes path with ffprobe (using the same hardening flags
// as the real conversion—see audioProbeArgs in convert.go) and rejects it
// unless validateAudioStreams accepts the result. Looks up ffprobe itself
// rather than taking a *converter, so this free function (like every
// other validateX in this file) doesn't need App-level wiring threaded
// through validateUpload/validateSyntax just for this one format family.
func validateAudio(format, path string) error {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return errors.New("audio validation is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), audioProbeTimeout)
	defer cancel()
	args := append([]string{}, audioProbeArgs...)
	args = append(args, "-f", format, "-i", path, "-show_streams", "-show_format", "-of", "json")
	cmd := exec.CommandContext(ctx, ffprobe, args...)
	cmd.Env = []string{"PATH=" + filepath.Dir(ffprobe)}
	stdout, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("invalid audio structure: %w", err)
	}
	var probe ffprobeOutput
	if err := json.Unmarshal(stdout, &probe); err != nil {
		return fmt.Errorf("could not parse audio probe output: %w", err)
	}
	return validateAudioStreams(format, probe)
}

// validateAudioStreams is validateAudio's decision logic, factored out as
// a pure function over an already-parsed probe result so it can be unit
// tested against hand-written fixtures instead of needing a real ffprobe
// binary. Accepts exactly one audio stream whose codec is on
// audioCodecWhitelist for format, plus—since real-world MP3/FLAC/Ogg
// files very commonly carry embedded cover art, which ffprobe reports as
// a "video" stream—any number of attached-picture streams
// (disposition.attached_pic == 1). Rejects anything else outright: a real
// (non-attached-pic) video stream, a subtitle or data stream, more than
// one audio stream, or a codec not on the whitelist. Also enforces
// maxAudioDurationSeconds against the container's own reported duration.
func validateAudioStreams(format string, probe ffprobeOutput) error {
	allowed := audioCodecWhitelist[format]
	audioStreams := 0
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "audio":
			audioStreams++
			if !allowed[s.CodecName] {
				return fmt.Errorf("audio codec %q is not accepted for %s input", s.CodecName, format)
			}
		case "video":
			if s.Disposition["attached_pic"] != 1 {
				return errors.New("audio file contains a video stream, which is not accepted")
			}
		default:
			return fmt.Errorf("audio file contains a %s stream, which is not accepted", s.CodecType)
		}
	}
	if audioStreams != 1 {
		return fmt.Errorf("expected exactly one audio stream, found %d", audioStreams)
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil {
		return errors.New("could not determine audio duration")
	}
	if duration <= 0 || duration > maxAudioDurationSeconds {
		return fmt.Errorf("audio duration of %.0fs is invalid or exceeds the %ds limit", duration, maxAudioDurationSeconds)
	}
	return nil
}

// validatePDFActiveContent rejects a PDF carrying a mechanism that runs
// code or exfiltrates data automatically the moment a document-processing
// tool opens it, without needing any user interaction: an embedded
// JavaScript name tree, an embedded file attachment (via the
// /EmbeddedFiles name tree, ctx.ListAttachments(), or an annotation's own
// /FS/EF—see validatePDFAnnotationAttachments), or a document-level
// /OpenAction or /AA (additional actions) entry on the catalog. This is a
// best-effort pass, not an exhaustive one—matching this codebase's
// existing honesty about narrower-than-ideal reject-lists (see e.g.
// rejectActiveContent's "<?php"/"<?=" pair): it rejects attachments
// wherever they're actually declared (the Names tree and every page's
// annotations), but still deliberately does not walk every annotation/
// form-field looking for a per-object ACTION dict (a Link annotation's
// own /A, or a form field's own /AA)—unlike an attachment, which is
// inert data sitting in the file regardless of interaction, those actions
// only fire on explicit user interaction (clicking a link, editing a
// field) that this service's pdftoppm-based rendering pipeline never
// performs. Verified against pdfcpu's own validation source
// (pkg/pdfcpu/validate/xReftable.go's validateNames, which is what
// populates XRefTable.Names["JavaScript"]/["EmbeddedFiles"] during
// Validate/ReadAndValidate) rather than guessed.
func validatePDFActiveContent(ctx *model.Context) error {
	if ctx.Names["JavaScript"] != nil {
		return errors.New("PDF contains embedded JavaScript, which is not accepted")
	}
	if ctx.Names["EmbeddedFiles"] != nil {
		return errors.New("PDF contains an embedded file attachment, which is not accepted")
	}
	attachments, err := ctx.ListAttachments()
	if err != nil {
		return fmt.Errorf("could not check PDF for embedded file attachments: %w", err)
	}
	if len(attachments) > 0 {
		return errors.New("PDF contains an embedded file attachment, which is not accepted")
	}
	if err := validatePDFAnnotationAttachments(ctx); err != nil {
		return err
	}
	if ctx.RootDict.HasEntry("OpenAction") {
		return errors.New("PDF contains a document open action, which is not accepted")
	}
	if ctx.RootDict.HasEntry("AA") {
		return errors.New("PDF document-level additional actions are not accepted")
	}
	return nil
}

// validatePDFAnnotationAttachments rejects a file attached via a page
// annotation's own /FS (file specification) entry—most commonly a
// /Subtype /FileAttachment annotation, PDF's "paperclip icon" attachment,
// but checked generically for any annotation carrying a Filespec with an
// /EF (embedded file) entry, since that combination is what actually
// makes a Filespec an embedded-file reference regardless of which
// annotation subtype carries it. Neither the /EmbeddedFiles name tree nor
// ctx.ListAttachments() covers this: both only look at the catalog's
// Names dictionary, and a page annotation's /FS never has to be
// registered there at all (temuan review P2)—this is genuinely inert
// attached data sitting in the file the moment it's opened, not an
// action that needs a click to reach, so it's in scope even though this
// codebase deliberately doesn't walk per-annotation ACTION dicts (see
// validatePDFActiveContent's doc comment).
func validatePDFAnnotationAttachments(ctx *model.Context) error {
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, false)
		if err != nil {
			return fmt.Errorf("could not read PDF page %d: %w", pageNr, err)
		}
		annotsObj, ok := pageDict.Find("Annots")
		if !ok {
			continue
		}
		annots, err := ctx.DereferenceArray(annotsObj)
		if err != nil {
			return fmt.Errorf("could not read PDF page %d annotations: %w", pageNr, err)
		}
		for _, annotObj := range annots {
			annotDict, err := ctx.DereferenceDict(annotObj)
			if err != nil {
				return fmt.Errorf("could not read PDF page %d annotation: %w", pageNr, err)
			}
			fsObj, ok := annotDict.Find("FS")
			if !ok {
				continue
			}
			fsDict, err := ctx.DereferenceDict(fsObj)
			if err != nil {
				return fmt.Errorf("could not read PDF page %d annotation file specification: %w", pageNr, err)
			}
			if fsDict.HasEntry("EF") {
				return errors.New("PDF contains an embedded file attachment, which is not accepted")
			}
		}
	}
	return nil
}

// odfMimeType is the exact, required value of an ODF package's mandatory
// first "mimetype" entry for each ODF format this codebase accepts. Used
// both by isODFMimetypeEntry (a raw byte-offset check against the
// upload's header, mirroring the fixed-signature checks already done for
// PNG/JPEG/OOXML) and by validateODF (re-checked against the actual ZIP
// entry once the package is opened structurally).
var odfMimeType = map[string]string{
	"odt": "application/vnd.oasis.opendocument.text",
	"ods": "application/vnd.oasis.opendocument.spreadsheet",
	"odp": "application/vnd.oasis.opendocument.presentation",
}

// isODFMimetypeEntry checks that head—the file's first bytes—begins with
// a ZIP local file header for an uncompressed "mimetype" entry (no extra
// field) whose content is exactly want. The ODF package format requires
// this exact layout: "mimetype" must be the package's first entry, stored
// rather than deflated, with no extra field—which is precisely what lets
// a content sniffer check a fixed byte offset directly, without opening
// the file as a ZIP archive first, the same way this codebase already
// checks a fixed PNG/JPEG signature. (Verified against the OASIS
// OpenDocument Package specification's own mimetype-file requirements,
// not guessed.)
func isODFMimetypeEntry(head []byte, want string) bool {
	const name = "mimetype"
	if len(head) < 30+len(name) || want == "" {
		return false
	}
	nameLen := int(binary.LittleEndian.Uint16(head[26:28]))
	extraLen := int(binary.LittleEndian.Uint16(head[28:30]))
	if nameLen != len(name) || extraLen != 0 {
		return false
	}
	if string(head[30:30+len(name)]) != name {
		return false
	}
	dataStart := 30 + len(name)
	dataEnd := dataStart + len(want)
	return len(head) >= dataEnd && string(head[dataStart:dataEnd]) == want
}

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
	{"image/gif", func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
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
	if len(zr.File) == 0 || len(zr.File) > maxZIPPackageEntries {
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
		if total > maxZIPPackageUncompressedSize || zeroSizeMismatch || (f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > 200) {
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

// odfXLinkNS, odfTextNS, and odfDrawNS are the fixed namespace URIs ODF
// uses for its xlink:href/text:a/draw:a elements (ODF 1.2/1.3 Part 1
// schema). A document is free to bind any local prefix to these—only the
// resolved namespace is meaningful, which is what encoding/xml gives us
// via Name.Space rather than a literal prefix string match.
const (
	odfXLinkNS = "http://www.w3.org/1999/xlink"
	odfTextNS  = "urn:oasis:names:tc:opendocument:xmlns:text:1.0"
	odfDrawNS  = "urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"
)

// odfObjectDirPattern matches an embedded object subpackage's directory,
// e.g. "Object1/", ODF's convention for an embedded OLE/foreign-format
// object (verified against real ODT/ODS files' own package layout, not
// guessed)—the ODF counterpart to OOXML's "/embeddings/" path.
var odfObjectDirPattern = regexp.MustCompile(`(?i)^object[0-9]+/`)

// validateODF is validateOOXML's counterpart for the other ZIP-based
// document package family this codebase accepts. It shares the same
// package-safety bounds (entry count/size, zip-slip, decompression ratio)
// and the same reject-outright stance on macros and embedded objects,
// adapted to ODF's own layout: Basic/Python macros live under top-level
// Basic/ or Scripts/ directories (verified against the OpenOffice/
// LibreOffice Basic IDE's own documented library storage convention, not
// guessed) rather than a vbaProject.bin part, and an embedded object is a
// numbered ObjectN/ subpackage (odfObjectDirPattern) rather than an
// /embeddings/ part.
//
// External resource references get the same default-deny treatment
// validateHTMLForPDF settled on after temuan review P1, rather than
// OOXML's narrower denylist-of-external-non-hyperlink-resources approach:
// the ODF spec resolves a relative xlink:href "by the method of XML
// Base" (ordinary RFC 3986 relative-reference resolution), and this
// codebase found no unambiguous confirmation that every ODF-consuming
// code path in LibreOffice resolves that purely against the package's
// own entries rather than ever falling through to the filesystem the way
// the HTML/Markdown->PDF path turned out to (temuan review P1 again)—so
// rather than assume, every xlink:href outside a text:a/draw:a hyperlink
// (never fetched, same allowance already made elsewhere in this
// codebase) is accepted only when it has no URI scheme, no ".."
// traversal segment, no leading "/", and actually names a real entry
// already present in this same, already-validated ZIP package: exactly
// the assets META-INF/manifest.xml itself already declares, never
// anything resolved outside the package.
func validateODF(format, path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return errors.New("invalid ODF ZIP structure")
	}
	defer zr.Close()
	if len(zr.File) == 0 || len(zr.File) > maxZIPPackageEntries {
		return errors.New("ODF package has an unsafe number of entries")
	}
	entries := make(map[string]bool, len(zr.File))
	var contentParts []*zip.File
	foundMimetype, foundManifest, foundContent := false, false, false
	var total uint64
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		clean := filepath.ToSlash(filepath.Clean(name))
		if name == "" || strings.HasPrefix(name, "/") || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, "../") {
			return errors.New("ODF package contains an unsafe path")
		}
		entries[clean] = true
		total += f.UncompressedSize64
		zeroSizeMismatch := f.CompressedSize64 == 0 && f.UncompressedSize64 != 0
		if total > maxZIPPackageUncompressedSize || zeroSizeMismatch || (f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > 200) {
			return errors.New("ODF package exceeds decompression safety limits")
		}
		lower := strings.ToLower(clean)
		if lower == "basic" || strings.HasPrefix(lower, "basic/") || lower == "scripts" || strings.HasPrefix(lower, "scripts/") || strings.HasSuffix(lower, ".xba") {
			return errors.New("ODF macros are not accepted")
		}
		if odfObjectDirPattern.MatchString(clean) {
			return errors.New("ODF embedded objects are not accepted")
		}
		switch clean {
		case "mimetype":
			foundMimetype = true
			rc, err := f.Open()
			if err != nil {
				return errors.New("invalid ODF mimetype part")
			}
			declared, err := io.ReadAll(io.LimitReader(rc, 256))
			_ = rc.Close()
			if err != nil || string(declared) != odfMimeType[format] {
				return errors.New("ODF mimetype does not match the declared format")
			}
		case "META-INF/manifest.xml":
			foundManifest = true
		case "content.xml":
			foundContent = true
			contentParts = append(contentParts, f)
		case "styles.xml":
			contentParts = append(contentParts, f)
		}
	}
	if !foundMimetype || !foundManifest || !foundContent {
		return errors.New("ODF package is missing required document parts")
	}
	for _, f := range contentParts {
		if err := validateODFResourceReferences(f, entries); err != nil {
			return err
		}
	}
	return nil
}

// validateODFResourceReferences scans f (content.xml or styles.xml) for
// every xlink:href attribute, on any element, and rejects it per
// validateODFResourceHref unless it's a text:a or draw:a element's own
// href—not any href found anywhere inside one, which would wrongly
// exempt an inner draw:image's real resource reference just because it
// happens to sit inside a hyperlinked frame. Uses a streaming xml.Decoder
// rather than pattern-matching the raw bytes for the same reason
// validateHTMLForPDF does:
// encoding/xml decodes character references in attribute values the same
// way a real XML consumer (LibreOffice's included) would, so a check
// against attr.Value here can't be bypassed by an encoded scheme the way
// a raw-byte regex could (temuan review P1).
func validateODFResourceReferences(f *zip.File, packageEntries map[string]bool) error {
	if f.UncompressedSize64 > maxZIPPackageUncompressedSize {
		return errors.New("ODF document part is too large")
	}
	r, err := f.Open()
	if err != nil {
		return errors.New("invalid ODF document part")
	}
	defer r.Close()
	dec := xml.NewDecoder(io.LimitReader(r, maxZIPPackageUncompressedSize))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid ODF document part XML")
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		// The hyperlink exemption applies only to THIS element's own href
		// (the link target, never fetched)—not to its descendants. A
		// draw:a can wrap a draw:frame/draw:image (an image that's also a
		// clickable link), and that inner image's href is a real resource
		// reference LibreOffice fetches during rendering regardless of the
		// hyperlink wrapper around it (temuan review P1: a previous
		// version tracked hyperlink-ness as a subtree flag that stayed set
		// for every descendant until the closing tag, which skipped
		// checking exactly that inner reference).
		isHyperlink := (el.Name.Space == odfTextNS || el.Name.Space == odfDrawNS) && el.Name.Local == "a"
		for _, attr := range el.Attr {
			if attr.Name.Space != odfXLinkNS || attr.Name.Local != "href" {
				continue
			}
			if isHyperlink {
				continue
			}
			if err := validateODFResourceHref(attr.Value, packageEntries); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateODFResourceHref accepts href only when it's empty, an in-
// document fragment ("#..."), or a package-relative path with no URI
// scheme/host, no ".." traversal segment, and no leading "/"—and which
// actually names a real entry already present in packageEntries. See
// validateODF's doc comment for why this is a default-deny allowlist
// against the package's own real contents rather than a denylist of
// external URL schemes.
//
// The match is done against u.Path, url.Parse's already percent-decoded
// form, not the raw href text: an href referencing a package entry whose
// name has a space or other reserved character in it (e.g.
// "Pictures/my%20photo.png" pointing at the real entry "Pictures/my
// photo.png") is an entirely ordinary, spec-legal URI encoding an author's
// own tooling produces—not an attack—and packageEntries holds the actual,
// unencoded ZIP entry names, so comparing against the still-encoded href
// would reject a legitimate reference to a real asset (temuan review P2).
func validateODFResourceHref(href string, packageEntries map[string]bool) error {
	if href == "" || strings.HasPrefix(href, "#") {
		return nil
	}
	u, err := url.Parse(href)
	if err != nil || u.IsAbs() || u.Host != "" || u.Scheme != "" {
		return errors.New("ODF external resource references are not accepted")
	}
	decoded := u.Path
	clean := filepath.ToSlash(filepath.Clean(decoded))
	if strings.HasPrefix(decoded, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("ODF resource references outside the package are not accepted")
	}
	if !packageEntries[clean] {
		return errors.New("ODF resource reference does not match a real package entry")
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
	if odfFormats[format] {
		// http.DetectContentType has no ODF-specific sniffing of its own—it
		// only recognizes the generic ZIP signature every ODF package also
		// has—so it reports "application/zip", never the exact ODF media
		// type isODFMimetypeEntry already checked directly against the
		// package's own mandatory mimetype entry.
		return mime == "application/zip" || mime == "application/octet-stream" || mime == odfMimeType[format]
	}
	if audioFormats[format] {
		// Verified against Go's own net/http sniffer (sniff.go): it
		// recognizes an ID3-tagged mp3 as "audio/mpeg", RIFF/WAVE as
		// "audio/wave", and "OggS\x00" as "application/ogg", but has no
		// signature for FLAC at all (no "fLaC" entry in its table) and
		// doesn't recognize a bare-frame-sync mp3 (no ID3 tag) either—both
		// fall through to the generic "application/octet-stream", which is
		// why every case here accepts that too rather than just the one
		// "proper" MIME type.
		switch format {
		case "mp3":
			return mime == "audio/mpeg" || mime == "application/octet-stream"
		case "wav":
			return mime == "audio/wave" || mime == "application/octet-stream"
		case "flac":
			return mime == "application/octet-stream"
		case "ogg":
			return mime == "application/ogg" || mime == "application/octet-stream"
		}
	}
	return strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/xml" || mime == "application/octet-stream"
}
