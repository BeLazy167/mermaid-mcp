package render

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func TestContentKey(t *testing.T) {
	request := Request{Diagram: "graph TD; A-->B", Format: FormatPNG}
	first := ContentKey("bundle-a", request)
	if len(first) != 64 || first != ContentKey("bundle-a", request) {
		t.Fatalf("ContentKey() = %q", first)
	}
	for _, changed := range []struct {
		bundle  string
		request Request
	}{
		{bundle: "bundle-b", request: request},
		{bundle: "bundle-a", request: Request{Diagram: request.Diagram, Format: FormatSVG}},
		{bundle: "bundle-a", request: Request{Diagram: request.Diagram + " ", Format: FormatPNG}},
	} {
		if got := ContentKey(changed.bundle, changed.request); got == first {
			t.Fatalf("changed key = %q, want different from %q", got, first)
		}
	}
}

func TestValidateOutput(t *testing.T) {
	validPNG := testPNG(1, 1)
	invalidPNGColor := append([]byte(nil), validPNG...)
	invalidPNGColor[24] = 1
	binary.BigEndian.PutUint32(invalidPNGColor[29:33], crc32.ChecksumIEEE(invalidPNGColor[12:29]))
	tests := []struct {
		name   string
		format Format
		data   []byte
		max    int64
		code   ErrorCode
	}{
		{name: "png", format: FormatPNG, data: validPNG, max: 100},
		{name: "svg internal references", format: FormatSVG, data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>.x{fill:url(#gradient)}</style><a href="#local"><path marker-end="url(#arrow)"/></a></svg>`), max: 1_000},
		{name: "XML SVG", format: FormatSVG, data: []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><text>ok</text></svg>`), max: 1_000},
		{name: "bad PNG signature", format: FormatPNG, data: append([]byte("not png"), pngIEND...), max: 100, code: CodeInternal},
		{name: "truncated PNG", format: FormatPNG, data: []byte("\x89PNG\r\n\x1a\nbody"), max: 100, code: CodeInternal},
		{name: "invalid PNG color", format: FormatPNG, data: invalidPNGColor, max: 100, code: CodeInternal},
		{name: "wrong SVG namespace", format: FormatSVG, data: []byte(`<svg xmlns="https://example.com/not-svg"/>`), max: 100, code: CodeRenderRejected},
		{name: "script", format: FormatSVG, data: []byte(`<svg><script>alert(1)</script></svg>`), max: 100, code: CodeRenderRejected},
		{name: "foreign object", format: FormatSVG, data: []byte(`<svg><foreignObject><div>bad</div></foreignObject></svg>`), max: 100, code: CodeRenderRejected},
		{name: "event handler", format: FormatSVG, data: []byte(`<svg><path onclick="bad()"/></svg>`), max: 100, code: CodeRenderRejected},
		{name: "animation", format: FormatSVG, data: []byte(`<svg><set attributeName="href" to="https://example.com"/></svg>`), max: 100, code: CodeRenderRejected},
		{name: "external href", format: FormatSVG, data: []byte(`<svg><a href="https://example.com">bad</a></svg>`), max: 100, code: CodeRenderRejected},
		{name: "data URL", format: FormatSVG, data: []byte(`<svg><image href="data:image/png;base64,x"/></svg>`), max: 100, code: CodeRenderRejected},
		{name: "external CSS URL", format: FormatSVG, data: []byte(`<svg><style>.x{background:url(https://example.com/x)}</style></svg>`), max: 100, code: CodeRenderRejected},
		{name: "CSS import", format: FormatSVG, data: []byte(`<svg><style>@import "https://example.com/x";</style></svg>`), max: 100, code: CodeRenderRejected},
		{name: "escaped CSS URL", format: FormatSVG, data: []byte(`<svg><style>.x{background:u\72l(https://example.com/x)}</style></svg>`), max: 200, code: CodeRenderRejected},
		{name: "CSS image set", format: FormatSVG, data: []byte(`<svg><style>.x{background:image-set("relative.png" 1x)}</style></svg>`), max: 200, code: CodeRenderRejected},
		{name: "XML stylesheet", format: FormatSVG, data: []byte(`<?xml-stylesheet href="https://example.com/x.css"?><svg/>`), max: 200, code: CodeRenderRejected},
		{name: "external XML base", format: FormatSVG, data: []byte(`<svg xml:base="https://example.com/"><image href="#x"/></svg>`), max: 200, code: CodeRenderRejected},
		{name: "external paint URL", format: FormatSVG, data: []byte(`<svg><path fill="url(https://example.com/x.svg#g)"/></svg>`), max: 200, code: CodeRenderRejected},
		{name: "Unicode before URL", format: FormatSVG, data: []byte(`<svg><style>.İ{fill:red}.x{fill:url(https://example.com/x)}</style></svg>`), max: 200, code: CodeRenderRejected},
		{name: "DTD", format: FormatSVG, data: []byte(`<!DOCTYPE svg><svg/>`), max: 100, code: CodeRenderRejected},
		{name: "oversized PNG dimensions", format: FormatPNG, data: testPNG(9000, 1), max: 100, code: CodeTooLarge},
		{name: "oversized SVG viewbox", format: FormatSVG, data: []byte(`<svg viewBox="0 0 9000 1"/>`), max: 100, code: CodeTooLarge},
		{name: "oversized SVG pixels", format: FormatSVG, data: []byte(`<svg viewBox="0 0 5000 5000"/>`), max: 100, code: CodeTooLarge},
		{name: "oversized", format: FormatSVG, data: []byte(`<svg/>`), max: 1, code: CodeTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateOutput(tt.format, tt.data, tt.max)
			if tt.code == "" && err != nil {
				t.Fatalf("validateOutput() error = %v", err)
			}
			if tt.code != "" {
				var renderErr *Error
				if !errors.As(err, &renderErr) || renderErr.Code != tt.code {
					t.Fatalf("validateOutput() error = %v, want code %q", err, tt.code)
				}
			}
		})
	}
}

func TestSanitizeOutputRemovesForeignObjectFallback(t *testing.T) {
	input := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><switch><foreignObject><div xmlns="http://www.w3.org/1999/xhtml">unsafe</div></foreignObject><text>safe fallback</text></switch></svg>`)
	got, err := sanitizeOutput(FormatSVG, input)
	if err != nil {
		t.Fatalf("sanitizeOutput() error = %v", err)
	}
	if bytes.Contains(bytes.ToLower(got), []byte("foreignobject")) || !bytes.Contains(got, []byte("safe fallback")) {
		t.Fatalf("sanitizeOutput() = %s", got)
	}
	if err := validateOutput(FormatSVG, got, int64(len(got))); err != nil {
		t.Fatalf("sanitized SVG error = %v", err)
	}
}

func TestValidateOutputRejectsEmpty(t *testing.T) {
	if err := validateOutput(FormatSVG, nil, 100); err == nil {
		t.Fatal("validateOutput() error = nil")
	}
	if bytes.HasSuffix([]byte("not a png"), pngIEND) {
		t.Fatal("test fixture unexpectedly valid")
	}
}

func testPNG(width, height uint32) []byte {
	result := append([]byte(nil), []byte("\x89PNG\r\n\x1a\n")...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8], ihdr[9], ihdr[10], ihdr[11], ihdr[12] = 8, 6, 0, 0, 0
	result = appendPNGChunk(result, "IHDR", ihdr)
	return appendPNGChunk(result, "IEND", nil)
}

func appendPNGChunk(target []byte, kind string, data []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	target = append(target, length[:]...)
	target = append(target, kind...)
	target = append(target, data...)
	checksum := crc32.ChecksumIEEE(target[len(target)-len(data)-4:])
	binary.BigEndian.PutUint32(length[:], checksum)
	return append(target, length[:]...)
}
