package app

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// writeZIP is a small helper shared by the fixture builders below: it
// writes each entry of parts as one ZIP member and returns the path to the
// resulting file (named input.bin, matching the opaque name real job
// storage uses—LibreOffice's filter detection relies on convertOffice
// staging it under its real extension, not on this path's name).
func writeZIP(t *testing.T, parts map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
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

// writeMinimalDOCX builds a real, LibreOffice-openable single-paragraph
// DOCX—not the placeholder-content fixture writeOOXMLFixture in
// office_test.go uses for validateUpload-only tests, which never reaches
// an actual document parser. Structure cross-checked against the OOXML
// package conventions (Content_Types Overrides, package .rels, WordprocessingML
// namespace) documented for minimal valid Office packages.
func writeMinimalDOCX(t *testing.T) string {
	t.Helper()
	return writeZIP(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`,
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:r><w:t>Regression corpus fixture with a heading-length line of body text.</w:t></w:r></w:p>
<w:sectPr/>
</w:body>
</w:document>`,
	})
}

// writeMinimalXLSX builds a real, LibreOffice-openable XLSX with
// sheetCount sheets, each holding one inline-string cell (no
// sharedStrings.xml needed). Structure verified against a known-good
// minimal-XLSX reference (workbook.xml <sheets>, xl/_rels/workbook.xml.rels
// mapping each sheet's r:id, one worksheets/sheetN.xml per sheet).
func writeMinimalXLSX(t *testing.T, sheetCount int) string {
	t.Helper()
	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`
	sheetsXML, relsXML := "", ""
	parts := map[string]string{}
	for i := 1; i <= sheetCount; i++ {
		contentTypes += fmt.Sprintf(`
<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
		sheetsXML += fmt.Sprintf(`<sheet name="Sheet%d" sheetId="%d" r:id="rId%d"/>`, i, i, i)
		relsXML += fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
		parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i)] = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Sheet %d content</t></is></c></row></sheetData>
</worksheet>`, i)
	}
	contentTypes += "\n</Types>"
	parts["[Content_Types].xml"] = contentTypes
	parts["_rels/.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`
	parts["xl/workbook.xml"] = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets>%s</sheets>
</workbook>`, sheetsXML)
	parts["xl/_rels/workbook.xml.rels"] = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">%s</Relationships>`, relsXML)
	return writeZIP(t, parts)
}

// writeMinimalPPTX builds a real, LibreOffice-openable single-slide
// PPTX: the roadmap's own previously-blocking complaint about this
// format specifically ("a slide -> slideLayout -> slideMaster -> theme
// relationship chain with real content in each") is exactly what this
// builds, all four parts plus their .rels wiring. This was genuinely
// unverifiable without a real LibreOffice to check a hand-built fixture
// against—which this project's Windows dev environment never had—so it
// was iterated against the actual `soffice`/`impress_pdf_Export` this
// codebase's own convertOffice invokes, running inside this project's
// own Docker image (which bundles libreoffice-impress), rather than
// assumed correct from the ECMA-376 spec alone: the version below is
// the one confirmed, by real conversion, to import cleanly and render.
// theme1.xml needs a real 12-color scheme, a font scheme, and all three
// tiers of the format scheme (fill/line/effect)—an empty or
// placeholder theme was tried first and rejected outright.
func writeMinimalPPTX(t *testing.T) string {
	t.Helper()
	return writeZIP(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
<Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>
<Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>
<Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>
<Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>
</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
</Relationships>`,
		"ppt/presentation.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
<p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>
<p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst>
<p:sldSz cx="9144000" cy="6858000"/>
<p:notesSz cx="6858000" cy="9144000"/>
</p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/>
</Relationships>`,
		"ppt/theme/theme1.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="Fixture Theme">
<a:themeElements>
<a:clrScheme name="Fixture">
<a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1>
<a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1>
<a:dk2><a:srgbClr val="1F497D"/></a:dk2>
<a:lt2><a:srgbClr val="EEECE1"/></a:lt2>
<a:accent1><a:srgbClr val="4F81BD"/></a:accent1>
<a:accent2><a:srgbClr val="C0504D"/></a:accent2>
<a:accent3><a:srgbClr val="9BBB59"/></a:accent3>
<a:accent4><a:srgbClr val="8064A2"/></a:accent4>
<a:accent5><a:srgbClr val="4BACC6"/></a:accent5>
<a:accent6><a:srgbClr val="F79646"/></a:accent6>
<a:hlink><a:srgbClr val="0000FF"/></a:hlink>
<a:folHlink><a:srgbClr val="800080"/></a:folHlink>
</a:clrScheme>
<a:fontScheme name="Fixture">
<a:majorFont><a:latin typeface="Calibri"/><a:ea typeface=""/><a:cs typeface=""/></a:majorFont>
<a:minorFont><a:latin typeface="Calibri"/><a:ea typeface=""/><a:cs typeface=""/></a:minorFont>
</a:fontScheme>
<a:fmtScheme name="Fixture">
<a:fillStyleLst>
<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
</a:fillStyleLst>
<a:lnStyleLst>
<a:ln w="6350"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln>
<a:ln w="12700"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln>
<a:ln w="19050"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:ln>
</a:lnStyleLst>
<a:effectStyleLst>
<a:effectStyle><a:effectLst/></a:effectStyle>
<a:effectStyle><a:effectLst/></a:effectStyle>
<a:effectStyle><a:effectLst/></a:effectStyle>
</a:effectStyleLst>
<a:bgFillStyleLst>
<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>
</a:bgFillStyleLst>
</a:fmtScheme>
</a:themeElements>
</a:theme>`,
		"ppt/slideMasters/slideMaster1.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
<p:cSld>
<p:spTree>
<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
<p:grpSpPr/>
</p:spTree>
</p:cSld>
<p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>
<p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst>
</p:sldMaster>`,
		"ppt/slideMasters/_rels/slideMaster1.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme" Target="../theme/theme1.xml"/>
</Relationships>`,
		"ppt/slideLayouts/slideLayout1.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" type="blank">
<p:cSld name="Blank">
<p:spTree>
<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
<p:grpSpPr/>
</p:spTree>
</p:cSld>
<p:clrMapOvr><a:overrideClrMapping bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/></p:clrMapOvr>
</p:sldLayout>`,
		"ppt/slideLayouts/_rels/slideLayout1.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/>
</Relationships>`,
		"ppt/slides/slide1.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
<p:cSld>
<p:spTree>
<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
<p:grpSpPr/>
<p:sp>
<p:nvSpPr><p:cNvPr id="2" name="Title 1"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr/></p:nvSpPr>
<p:spPr/>
<p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>Regression corpus fixture slide</a:t></a:r></a:p></p:txBody>
</p:sp>
</p:spTree>
</p:cSld>
</p:sld>`,
		"ppt/slides/_rels/slide1.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
</Relationships>`,
	})
}

func officeLookPath(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("libreoffice"); err == nil {
		return p
	}
	if p, err := exec.LookPath("soffice"); err == nil {
		return p
	}
	t.Skip("libreoffice/soffice not installed, skipping Office conversion test")
	return ""
}

// TestConvertOfficeToStandardPDF is the baseline regression-corpus case
// this codebase didn't previously have at all: a real DOCX, a real
// multi-sheet XLSX, and a real PPTX (closing the roadmap's own
// previously-open "Impress/PPTX regression fixture" gap—see
// writeMinimalPPTX), all actually run through LibreOffice, producing a
// valid PDF. Skips where libreoffice/soffice isn't installed (this
// project's own Windows dev machine included, same as the
// poppler-dependent PDF tests), and runs for real in the Docker image
// and CI.
func TestConvertOfficeToStandardPDF(t *testing.T) {
	officeLookPath(t)
	c := newConverter()
	cases := []struct {
		name        string
		in          string
		build       func(*testing.T) string
		expectPages int
	}{
		{"docx", "docx", writeMinimalDOCX, 1},
		{"xlsx three sheets", "xlsx", func(t *testing.T) string { return writeMinimalXLSX(t, 3) }, 3},
		{"pptx", "pptx", writeMinimalPPTX, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inPath := tc.build(t)
			outPath := filepath.Join(t.TempDir(), "output.pdf")
			if err := c.run(context.Background(), tc.in, "pdf", "", inPath, outPath); err != nil {
				t.Fatalf("Office conversion failed: %v", err)
			}
			assertValidPDF(t, outPath)
		})
	}
}

// TestConvertXLSXOptimizedProducesOnePagePerSheet is the strongest fidelity
// assertion this slice can make with confidence: SinglePageSheets is
// documented to unconditionally put every sheet on exactly one page
// regardless of its content or print setup, so a 3-sheet workbook must
// come back as a 3-page PDF under pdfMode=optimized. (Standard mode's page
// count depends on each sheet's print area/paper size interacting with
// content width, which isn't something a hand-built fixture can reliably
// predict without a running LibreOffice to check against—so this test
// doesn't assert anything about standard mode's page count, only that
// optimized mode's documented guarantee holds.)
func TestConvertXLSXOptimizedProducesOnePagePerSheet(t *testing.T) {
	officeLookPath(t)
	c := newConverter()
	inPath := writeMinimalXLSX(t, 3)
	outPath := filepath.Join(t.TempDir(), "output.pdf")
	if err := c.run(context.Background(), "xlsx", "pdf", PDFModeOptimized, inPath, outPath); err != nil {
		t.Fatalf("Office conversion failed: %v", err)
	}
	count := assertValidPDF(t, outPath)
	if count != 3 {
		t.Fatalf("SinglePageSheets should put each of 3 sheets on its own page, got %d pages", count)
	}
}

// TestConvertOfficeOptimizedModeStillProducesValidPDF exercises the
// EmbedStandardFonts/ReduceImageResolution options (which apply uniformly
// across formats, unlike SinglePageSheets) on a DOCX: the strongest thing
// a fixture without real embedded fonts/images can verify is that the
// filter data is accepted and produces a well-formed PDF, not silently
// broken output.
func TestConvertOfficeOptimizedModeStillProducesValidPDF(t *testing.T) {
	officeLookPath(t)
	c := newConverter()
	inPath := writeMinimalDOCX(t)
	outPath := filepath.Join(t.TempDir(), "output.pdf")
	if err := c.run(context.Background(), "docx", "pdf", PDFModeOptimized, inPath, outPath); err != nil {
		t.Fatalf("Office conversion failed: %v", err)
	}
	assertValidPDF(t, outPath)
}

// assertValidPDF checks the output both structurally (pdfcpu, the same
// independent parser validateUpload uses for input) and via its own page
// count, returning the count for the caller to assert on.
func assertValidPDF(t *testing.T, path string) int {
	t.Helper()
	if err := api.ValidateFile(path, nil); err != nil {
		t.Fatalf("Office conversion produced a structurally invalid PDF: %v", err)
	}
	count, err := api.PageCountFile(path)
	if err != nil {
		t.Fatalf("could not read page count: %v", err)
	}
	if count < 1 {
		t.Fatal("expected at least one page")
	}
	return count
}

// ---- pure-Go coverage for pdfMode resolution and filter-data construction.
//
// These don't need libreoffice/soffice installed, so unlike the tests
// above they run for real on every machine, including this project's own
// Windows dev box. They're also the only local coverage for Impress/PPTX:
// building a minimal-but-schema-valid PPTX by hand (slide -> slideLayout
// -> slideMaster -> theme, each with their own required elements) turned
// out to need considerably more scaffolding than DOCX/XLSX, and with no
// local LibreOffice to verify a hand-built fixture actually opens, shipping
// one unverified risked a corpus test that's silently wrong rather than
// useful. PPTX shares 100% of the same conversion code (officePDFFilter*,
// convertOffice) as DOCX/XLSX—the only PPTX-specific fact is the filter
// name below, which is verified against LibreOffice's own filter registry
// (filter/source/config/fragments/filters/impress_pdf_Export.xcu) same as
// the other two.

func TestOfficePDFFilterNameCoversAllThreeFormats(t *testing.T) {
	want := map[string]string{"docx": "writer_pdf_Export", "xlsx": "calc_pdf_Export", "pptx": "impress_pdf_Export"}
	for format, filter := range want {
		if got := officePDFFilterName[format]; got != filter {
			t.Errorf("officePDFFilterName[%q] = %q, want %q", format, got, filter)
		}
	}
}

func TestOfficePDFFilterOptionsStandardModeIsEmpty(t *testing.T) {
	for _, in := range []string{"docx", "xlsx", "pptx"} {
		if got := officePDFFilterOptions(in, PDFModeStandard); got != "" {
			t.Errorf("officePDFFilterOptions(%q, standard) = %q, want empty (no filter data at all)", in, got)
		}
		if got := officePDFFilterOptions(in, ""); got != "" {
			t.Errorf("officePDFFilterOptions(%q, \"\") = %q, want empty", in, got)
		}
	}
}

func TestOfficePDFFilterOptionsOptimizedModeIsFormatSpecific(t *testing.T) {
	xlsx := officePDFFilterOptions("xlsx", PDFModeOptimized)
	if !strings.Contains(xlsx, `"SinglePageSheets":{"type":"boolean","value":"true"}`) {
		t.Errorf("expected xlsx optimized filter data to include SinglePageSheets, got %s", xlsx)
	}
	for _, in := range []string{"docx", "xlsx", "pptx"} {
		got := officePDFFilterOptions(in, PDFModeOptimized)
		for _, want := range []string{
			`"EmbedStandardFonts":{"type":"boolean","value":"true"}`,
			`"ReduceImageResolution":{"type":"boolean","value":"true"}`,
			`"MaxImageResolution":{"type":"long","value":"150"}`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("officePDFFilterOptions(%q, optimized) = %s, missing %s", in, got, want)
			}
		}
		if in != "xlsx" && strings.Contains(got, "SinglePageSheets") {
			t.Errorf("officePDFFilterOptions(%q, optimized) should not include the Calc-only SinglePageSheets, got %s", in, got)
		}
	}
}

func TestResolvePDFModeDefaultsToStandardForOfficeToPDF(t *testing.T) {
	got, err := resolvePDFMode("docx", "pdf", "")
	if err != nil || got != PDFModeStandard {
		t.Fatalf("resolvePDFMode(docx, pdf, \"\") = (%q, %v), want (%q, nil)", got, err, PDFModeStandard)
	}
}

func TestResolvePDFModeIsBlankWhenNotApplicable(t *testing.T) {
	got, err := resolvePDFMode("csv", "json", "")
	if err != nil || got != "" {
		t.Fatalf("resolvePDFMode(csv, json, \"\") = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestResolvePDFModeRejectsUnknownValue(t *testing.T) {
	if _, err := resolvePDFMode("docx", "pdf", "extreme"); err == nil {
		t.Fatal("expected an unrecognized pdfMode value to be rejected")
	}
}

func TestResolvePDFModeRejectsWhenNotApplicable(t *testing.T) {
	if _, err := resolvePDFMode("csv", "json", "optimized"); err == nil {
		t.Fatal("expected pdfMode to be rejected for a pair it doesn't apply to")
	}
	if _, err := resolvePDFMode("png", "pdf", "optimized"); err == nil {
		t.Fatal("expected pdfMode to be rejected for image->PDF (it only tunes the LibreOffice export filter)")
	}
	if _, err := resolvePDFMode("html", "pdf", "optimized"); err == nil {
		t.Fatal("expected pdfMode to be rejected for html->PDF (it also goes through LibreOffice, but has no per-format filter options)")
	}
	if _, err := resolvePDFMode("markdown", "pdf", "optimized"); err == nil {
		t.Fatal("expected pdfMode to be rejected for markdown->PDF")
	}
}

func TestResolvePDFModeIsCaseInsensitiveAndTrimmed(t *testing.T) {
	got, err := resolvePDFMode("xlsx", "pdf", "  Optimized  ")
	if err != nil || got != PDFModeOptimized {
		t.Fatalf("resolvePDFMode(xlsx, pdf, \"  Optimized  \") = (%q, %v), want (%q, nil)", got, err, PDFModeOptimized)
	}
}
