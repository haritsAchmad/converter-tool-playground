package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
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
	default:
		if bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(head) {
			return "", errors.New("text input must be valid UTF-8 without NUL bytes")
		}
	}
	if imageFormats[candidate] && candidate != "webp" {
		if _, _, err := image.DecodeConfig(bytes.NewReader(head)); err != nil {
			return "", errors.New("image header is invalid")
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
	return strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/xml" || mime == "application/octet-stream"
}
