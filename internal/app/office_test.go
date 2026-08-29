package app

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOfficeEnvironmentIncludesRequiredToolPaths(t *testing.T) {
	env := officeEnvironment(filepath.Join("/usr", "bin", "libreoffice"), t.TempDir())
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=") {
		t.Fatal("Office environment has no PATH")
	}
	if runtime.GOOS != "windows" && !strings.Contains(joined, ":/bin") {
		t.Fatalf("Office environment omits Alpine core utilities: %s", joined)
	}
}

func writeOOXMLFixture(t *testing.T, format string, extra map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		map[string]string{"docx": "word/document.xml", "xlsx": "xl/workbook.xml", "pptx": "ppt/presentation.xml"}[format]: `<?xml version="1.0"?><root/>`,
	}
	for name, body := range extra {
		parts[name] = body
	}
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateOOXMLFormats(t *testing.T) {
	for _, format := range []string{"docx", "xlsx", "pptx"} {
		t.Run(format, func(t *testing.T) {
			path := writeOOXMLFixture(t, format, nil)
			got, err := validateUpload(path, "document."+format)
			if err != nil {
				t.Fatalf("expected valid %s package: %v", format, err)
			}
			if got != format {
				t.Fatalf("got format %q, want %q", got, format)
			}
		})
	}
}

func TestValidateOOXMLRejectsWrongPackageFamily(t *testing.T) {
	path := writeOOXMLFixture(t, "docx", nil)
	if _, err := validateUpload(path, "renamed.xlsx"); err == nil {
		t.Fatal("expected DOCX renamed as XLSX to be rejected")
	}
}

func TestValidateOOXMLRejectsMacroAndTraversal(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"macro":     {"word/vbaProject.bin": "macro"},
		"traversal": {"../outside.xml": "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeOOXMLFixture(t, "docx", extra)
			if _, err := validateUpload(path, "document.docx"); err == nil {
				t.Fatal("expected unsafe OOXML package to be rejected")
			}
		})
	}
}

func TestValidateOOXMLRejectsExternalResourceButAllowsHyperlink(t *testing.T) {
	externalImage := `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="https://example.test/pixel.png" TargetMode="External"/></Relationships>`
	path := writeOOXMLFixture(t, "docx", map[string]string{"word/_rels/document.xml.rels": externalImage})
	if _, err := validateUpload(path, "document.docx"); err == nil {
		t.Fatal("expected external image relationship to be rejected")
	}

	hyperlink := `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="https://example.test/" TargetMode="External"/></Relationships>`
	path = writeOOXMLFixture(t, "docx", map[string]string{"word/_rels/document.xml.rels": hyperlink})
	if _, err := validateUpload(path, "document.docx"); err != nil {
		t.Fatalf("ordinary hyperlink should remain allowed: %v", err)
	}
}

func TestOfficeCapabilitiesDependOnLibreOffice(t *testing.T) {
	c := &converter{}
	if c.supports("docx", "pdf") {
		t.Fatal("Office conversion advertised without LibreOffice")
	}
	c.libreoffice = filepath.Join("tools", "libreoffice")
	for _, format := range []string{"docx", "xlsx", "pptx"} {
		if !c.supports(format, "pdf") {
			t.Fatalf("expected %s -> PDF support with LibreOffice", strings.ToUpper(format))
		}
	}
	if c.supports("pdf", "docx") {
		t.Fatal("PDF -> DOCX must not be advertised")
	}
}
