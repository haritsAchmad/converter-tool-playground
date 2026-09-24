package app

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCSVFormulaInjectionDefused guards against OWASP "CSV Injection": a
// cell opening with =, +, -, @, tab, or CR is treated as a formula by
// Excel/Sheets/LibreOffice when the exported file is later opened, which
// can run commands or exfiltrate data. Values (and headers) with those
// leading characters must come out prefixed with a quote so they stay
// inert text.
func TestCSVFormulaInjectionDefused(t *testing.T) {
	rows := []any{
		map[string]any{"name": "Ada", "note": "=cmd|' /C calc'!A0"},
		map[string]any{"name": "Bob", "note": "+1+1"},
		map[string]any{"name": "Cy", "note": "-2+3"},
		map[string]any{"name": "Di", "note": "@SUM(1,1)"},
		map[string]any{"name": "Eve", "note": "safe value"},
	}
	out, err := encodeCSV(rows)
	if err != nil {
		t.Fatal(err)
	}
	csvText := string(out)
	for _, dangerous := range []string{"=cmd|", "+1+1", "-2+3", "@SUM"} {
		if strings.Contains(csvText, dangerous) && !strings.Contains(csvText, "'"+dangerous) {
			t.Fatalf("formula value %q reached output unquoted:\n%s", dangerous, csvText)
		}
	}
	if !strings.Contains(csvText, "safe value") || strings.Contains(csvText, "'safe value") {
		t.Fatalf("benign value was unexpectedly altered:\n%s", csvText)
	}
}

func TestDefuseCSVFormula(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"=1+1":               "'=1+1",
		"+1":                 "'+1",
		"-1":                 "'-1",
		"@cmd":               "'@cmd",
		"\ttab":              "'\ttab",
		"\rcr":               "'\rcr",
		"normal":             "normal",
		"contains=elsewhere": "contains=elsewhere",
	}
	for in, want := range cases {
		if got := defuseCSVFormula(in); got != want {
			t.Errorf("defuseCSVFormula(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestYAMLAliasBombRejected locks in that gopkg.in/yaml.v3 refuses
// excessive anchor/alias expansion (a "billion laughs" style bomb) instead
// of exhausting memory decoding it, since this project relies on that
// upstream protection rather than implementing its own.
func TestYAMLAliasBombRejected(t *testing.T) {
	var b strings.Builder
	b.WriteString("a: &a [\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\"]\n")
	prev := "a"
	for i := 0; i < 9; i++ {
		name := string(rune('b' + i))
		b.WriteString(name + ": &" + name + " [*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + "]\n")
		prev = name
	}
	if _, err := decodeData("yaml", []byte(b.String())); err == nil {
		t.Fatal("expected alias bomb to be rejected, decode succeeded")
	}
}

// TestJSONDeepNestingRejected locks in that encoding/json's built-in
// nesting-depth guard protects the JSON input path from a stack-overflow
// style denial of service via a deeply nested array.
func TestJSONDeepNestingRejected(t *testing.T) {
	const depth = 100000
	var b strings.Builder
	b.Grow(depth*2 + 1)
	for i := 0; i < depth; i++ {
		b.WriteByte('[')
	}
	b.WriteByte('1')
	for i := 0; i < depth; i++ {
		b.WriteByte(']')
	}
	if _, err := decodeData("json", []byte(b.String())); err == nil {
		t.Fatal("expected deeply nested JSON to be rejected, decode succeeded")
	}
}

// pngHeader builds a minimal-but-valid PNG signature + IHDR chunk declaring
// the given dimensions, with no IDAT/IEND. image.DecodeConfig only needs
// IHDR to report width/height for this color type, so it's enough to drive
// validateUpload's dimension check without actually materializing a
// gigantic image on disk for the test.
func pngHeader(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // color type: truecolor with alpha
	writeChunk(t, &buf, "IHDR", ihdr)
	return buf.Bytes()
}

func writeChunk(t *testing.T, buf *bytes.Buffer, typ string, data []byte) {
	t.Helper()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	buf.Write(lenBuf[:])
	typeAndData := append([]byte(typ), data...)
	buf.Write(typeAndData)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(typeAndData))
	buf.Write(crcBuf[:])
}

// TestRejectsImageDimensionBomb guards against a "decompression bomb": a
// tiny PNG whose header lies about having an enormous pixel count, which
// would otherwise sail through the (small) upload size limit and then have
// image.Decode allocate a multi-gigabyte pixel buffer during conversion.
// validateUpload must reject it from the declared header alone, before any
// full decode is attempted.
func TestRejectsImageDimensionBomb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bomb.png")
	// 40000 x 40000 = 1.6 billion declared pixels, far past the 100M limit.
	if err := os.WriteFile(path, pngHeader(t, 40000, 40000), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "bomb.png"); err == nil {
		t.Fatal("expected oversized declared image dimensions to be rejected")
	}
}

// TestAcceptsReasonableImageDimensions locks in that the new dimension
// check doesn't collaterally reject ordinary, modestly sized images.
func TestAcceptsReasonableImageDimensions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.png")
	if err := os.WriteFile(path, pngHeader(t, 1920, 1080), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "ok.png"); err != nil {
		t.Fatalf("expected a reasonably sized image to pass validation, got %v", err)
	}
}
