package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMMDCValidateAndRender(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}

	tests := []struct {
		name       string
		request    Request
		maxDiagram int64
		wantData   string
		wantMIME   string
		wantCode   ErrorCode
	}{
		{
			name:       "renders svg",
			request:    Request{Diagram: "graph TD; A-->B", Format: FormatSVG},
			maxDiagram: 1_024,
			wantData:   "<svg>rendered</svg>",
			wantMIME:   "image/svg+xml",
		},
		{
			name:       "renders png",
			request:    Request{Diagram: "graph TD; A-->B", Format: FormatPNG},
			maxDiagram: 1_024,
			wantData:   "\x89PNG\r\n\x1a\nrendered",
			wantMIME:   "image/png",
		},
		{
			name:       "rejects empty source",
			request:    Request{Diagram: " \n\t", Format: FormatPNG},
			maxDiagram: 1_024,
			wantCode:   CodeInvalidInput,
		},
		{
			name:       "rejects oversized source",
			request:    Request{Diagram: strings.Repeat("x", 5), Format: FormatPNG},
			maxDiagram: 4,
			wantCode:   CodeTooLarge,
		},
		{
			name:       "rejects unsupported format",
			request:    Request{Diagram: "graph TD; A-->B", Format: Format("pdf")},
			maxDiagram: 1_024,
			wantCode:   CodeUnsupportedFormat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRoot := t.TempDir()
			helper := writeFakeMMDC(t, tempRoot)
			renderTemp := filepath.Join(tempRoot, "render-temp")
			if err := os.Mkdir(renderTemp, 0o700); err != nil {
				t.Fatalf("create render temp: %v", err)
			}

			renderer, err := NewMMDC(MMDCConfig{
				Path:            helper,
				TempDir:         renderTemp,
				MaxDiagramBytes: tt.maxDiagram,
				MaxOutputBytes:  1_024,
				Concurrency:     1,
				Timeout:         time.Second,
			})
			if err != nil {
				t.Fatalf("NewMMDC() error = %v", err)
			}

			result, err := renderer.Render(context.Background(), tt.request)
			if tt.wantCode != "" {
				assertErrorCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if string(result.Data) != tt.wantData {
				t.Fatalf("Render() data = %q, want %q", result.Data, tt.wantData)
			}
			if result.MIMEType != tt.wantMIME {
				t.Fatalf("Render() MIME = %q, want %q", result.MIMEType, tt.wantMIME)
			}

			entries, err := os.ReadDir(renderTemp)
			if err != nil {
				t.Fatalf("read render temp: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("temporary files remain: %v", entries)
			}
		})
	}
}

func TestNewMMDCRejectsInvalidConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}

	tempRoot := t.TempDir()
	helper := writeFakeMMDC(t, tempRoot)
	base := MMDCConfig{
		Path:            helper,
		MaxDiagramBytes: 1,
		MaxOutputBytes:  1,
		Concurrency:     1,
		Timeout:         time.Second,
	}
	tests := []struct {
		name   string
		change func(*MMDCConfig)
	}{
		{name: "missing executable", change: func(config *MMDCConfig) { config.Path = filepath.Join(tempRoot, "missing") }},
		{name: "invalid input limit", change: func(config *MMDCConfig) { config.MaxDiagramBytes = 0 }},
		{name: "invalid output limit", change: func(config *MMDCConfig) { config.MaxOutputBytes = 0 }},
		{name: "invalid concurrency", change: func(config *MMDCConfig) { config.Concurrency = 0 }},
		{name: "invalid timeout", change: func(config *MMDCConfig) { config.Timeout = 0 }},
		{name: "missing Mermaid config", change: func(config *MMDCConfig) {
			config.MermaidConfigPath = filepath.Join(tempRoot, "missing.json")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := base
			tt.change(&config)
			if _, err := NewMMDC(config); err == nil {
				t.Fatal("NewMMDC() error = nil")
			}
		})
	}
}

func TestMMDCRenderOutputValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}

	tests := []struct {
		name      string
		diagram   string
		maxOutput int64
		wantCode  ErrorCode
	}{
		{name: "oversized output", diagram: "OVERSIZE", maxOutput: 8, wantCode: CodeTooLarge},
		{name: "invalid output", diagram: "INVALID", maxOutput: 1_024, wantCode: CodeInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRoot := t.TempDir()
			renderer, err := NewMMDC(MMDCConfig{
				Path:            writeFakeMMDC(t, tempRoot),
				TempDir:         tempRoot,
				MaxDiagramBytes: 1_024,
				MaxOutputBytes:  tt.maxOutput,
				Concurrency:     1,
				Timeout:         time.Second,
			})
			if err != nil {
				t.Fatalf("NewMMDC() error = %v", err)
			}

			_, err = renderer.Render(context.Background(), Request{Diagram: tt.diagram, Format: FormatPNG})
			assertErrorCode(t, err, tt.wantCode)
		})
	}
}

func TestPublicMessageAndMIMEType(t *testing.T) {
	if got := PublicMessage(errors.New("secret")); got != "rendering failed" {
		t.Fatalf("PublicMessage() = %q", got)
	}
	if got := FormatPNG.MIMEType(); got != "image/png" {
		t.Fatalf("PNG MIME type = %q", got)
	}
	if got := FormatSVG.MIMEType(); got != "image/svg+xml" {
		t.Fatalf("SVG MIME type = %q", got)
	}
}

func TestMMDCRenderFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}

	tests := []struct {
		name     string
		diagram  string
		timeout  time.Duration
		wantCode ErrorCode
	}{
		{name: "renderer rejection", diagram: "FAIL", timeout: time.Second, wantCode: CodeRenderRejected},
		{name: "renderer timeout", diagram: "SLOW", timeout: 20 * time.Millisecond, wantCode: CodeTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRoot := t.TempDir()
			renderer, err := NewMMDC(MMDCConfig{
				Path:            writeFakeMMDC(t, tempRoot),
				TempDir:         tempRoot,
				MaxDiagramBytes: 1_024,
				MaxOutputBytes:  1_024,
				Concurrency:     1,
				Timeout:         tt.timeout,
			})
			if err != nil {
				t.Fatalf("NewMMDC() error = %v", err)
			}

			_, err = renderer.Render(context.Background(), Request{Diagram: tt.diagram, Format: FormatSVG})
			assertErrorCode(t, err, tt.wantCode)
			if strings.Contains(err.Error(), tempRoot) {
				t.Fatalf("error leaks temporary path: %v", err)
			}
			entries, readErr := os.ReadDir(tempRoot)
			if readErr != nil {
				t.Fatalf("read temp root: %v", readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "mermaid-render-") {
					t.Fatalf("temporary directory remains: %s", entry.Name())
				}
			}
		})
	}
}

func TestSanitizeRendererMessageRemovesStackAndPaths(t *testing.T) {
	tempDir := "/tmp/mermaid-render-secret"
	message := "UnknownDiagramError in " + tempDir + "/diagram.mmd\n" +
		"detectType (https://mermaid-cli-intercept.invalid/chunk.js:10:2)\n" +
		"    at renderMermaid (file:///home/mermaidcli/index.js:2:1)"

	got := sanitizeRendererMessage(message, tempDir)

	if got != "UnknownDiagramError in <temp>/diagram.mmd" {
		t.Fatalf("sanitizeRendererMessage() = %q", got)
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		input string
		want  Format
		err   bool
	}{
		{input: "", want: FormatPNG},
		{input: "png", want: FormatPNG},
		{input: "SVG", want: FormatSVG},
		{input: "pdf", err: true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseFormat(tt.input)
			if (err != nil) != tt.err {
				t.Fatalf("ParseFormat(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseFormat(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func assertErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", want)
	}
	var renderErr *Error
	if !errors.As(err, &renderErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if renderErr.Code != want {
		t.Fatalf("error code = %q, want %q (error: %v)", renderErr.Code, want, err)
	}
}

func writeFakeMMDC(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-mmdc")
	script := `#!/bin/sh
set -eu
input=""
output=""
format=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --input) input="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    --outputFormat) format="$2"; shift 2 ;;
    --configFile|--puppeteerConfigFile) shift 2 ;;
    --quiet) shift ;;
    *) echo "unexpected argument: $1" >&2; exit 9 ;;
  esac
done
if grep -q FAIL "$input"; then
  echo "bad diagram in $input" >&2
  exit 2
fi
if grep -q SLOW "$input"; then
  sleep 2
fi
if grep -q OVERSIZE "$input"; then
  dd if=/dev/zero of="$output" bs=2048 count=1 2>/dev/null
  exit 0
fi
if grep -q INVALID "$input"; then
  printf 'not-an-image' > "$output"
  exit 0
fi
if [ "$format" = "png" ]; then
  printf '\211PNG\r\n\032\nrendered' > "$output"
else
  printf '<svg>rendered</svg>' > "$output"
fi
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake mmdc: %v", err)
	}
	return path
}
