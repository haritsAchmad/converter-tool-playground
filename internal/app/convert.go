package app

import (
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
)

type converter struct{ magick string }

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
}

var dataFormats = map[string]bool{"csv": true, "json": true, "xml": true, "yaml": true}
var imageFormats = map[string]bool{"png": true, "jpeg": true, "webp": true}

func newConverter() *converter { p, _ := exec.LookPath("magick"); return &converter{magick: p} }

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
	if imageFormats[in] && imageFormats[out] {
		if in == "webp" || out == "webp" {
			return c.magick != ""
		}
		return true
	}
	return false
}

func (c *converter) run(ctx context.Context, in, out, inPath, outPath string) error {
	if !c.supports(in, out) {
		return errors.New("conversion pair is not supported")
	}
	if dataFormats[in] {
		return convertData(in, out, inPath, outPath)
	}
	if imageFormats[in] {
		return c.convertImage(ctx, in, out, inPath, outPath)
	}
	return convertDocument(in, out, inPath, outPath)
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
	_ = w.Write(head)
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
			row[i] = scalarString(val)
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
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
