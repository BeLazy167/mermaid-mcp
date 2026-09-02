// Package render converts Mermaid source into image bytes.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Format is a supported image output format.
type Format string

const (
	// FormatPNG renders a raster PNG image.
	FormatPNG Format = "png"
	// FormatSVG renders a vector SVG image.
	FormatSVG Format = "svg"
)

// ErrorCode identifies a rendering failure category.
type ErrorCode string

const (
	// CodeInvalidInput means the Mermaid source is empty or invalid UTF-8.
	CodeInvalidInput ErrorCode = "invalid_input"
	// CodeTooLarge means an input or output exceeded its configured limit.
	CodeTooLarge ErrorCode = "too_large"
	// CodeUnsupportedFormat means the requested image format is unsupported.
	CodeUnsupportedFormat ErrorCode = "unsupported_format"
	// CodeUnsupportedMediaType means the HTTP request body type is unsupported.
	CodeUnsupportedMediaType ErrorCode = "unsupported_media_type"
	// CodeRenderRejected means Mermaid CLI could not render the diagram.
	CodeRenderRejected ErrorCode = "render_rejected"
	// CodeTimeout means rendering exceeded its deadline.
	CodeTimeout ErrorCode = "timeout"
	// CodeCanceled means the caller canceled rendering.
	CodeCanceled ErrorCode = "canceled"
	// CodeInternal means the renderer or local environment failed.
	CodeInternal ErrorCode = "internal"
)

// Error is a rendering error safe to classify at API boundaries.
type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}

// Error returns the public message plus the underlying cause for server logs.
func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

// Unwrap exposes the underlying error.
func (e *Error) Unwrap() error {
	return e.Cause
}

// PublicMessage returns an error message safe to send to a caller.
func PublicMessage(err error) string {
	var renderErr *Error
	if errors.As(err, &renderErr) && renderErr.Message != "" {
		return renderErr.Message
	}
	return "rendering failed"
}

// Request describes one diagram render.
type Request struct {
	Diagram string
	Format  Format
}

// Result contains a rendered image and its media type.
type Result struct {
	Data     []byte
	MIMEType string
}

// Renderer renders Mermaid diagrams.
type Renderer interface {
	Render(context.Context, Request) (Result, error)
}

// MMDCConfig configures a Mermaid CLI renderer.
type MMDCConfig struct {
	Path                string
	PuppeteerConfigPath string
	MermaidConfigPath   string
	TempDir             string
	MaxDiagramBytes     int64
	MaxOutputBytes      int64
	Concurrency         int
	Timeout             time.Duration
}

// MMDC invokes Mermaid CLI in a bounded subprocess pool.
type MMDC struct {
	path                string
	puppeteerConfigPath string
	mermaidConfigPath   string
	tempDir             string
	maxDiagramBytes     int64
	maxOutputBytes      int64
	timeout             time.Duration
	slots               chan struct{}
}

// NewMMDC validates config and creates a Mermaid CLI renderer.
func NewMMDC(config MMDCConfig) (*MMDC, error) {
	if config.Path == "" {
		config.Path = "mmdc"
	}
	path, err := exec.LookPath(config.Path)
	if err != nil {
		return nil, fmt.Errorf("find Mermaid CLI %q: %w", config.Path, err)
	}
	if config.MaxDiagramBytes <= 0 {
		return nil, errors.New("MaxDiagramBytes must be positive")
	}
	if config.MaxOutputBytes <= 0 {
		return nil, errors.New("MaxOutputBytes must be positive")
	}
	if config.Concurrency <= 0 {
		return nil, errors.New("concurrency must be positive")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	for name, path := range map[string]string{
		"PuppeteerConfigPath": config.PuppeteerConfigPath,
		"MermaidConfigPath":   config.MermaidConfigPath,
	} {
		if path == "" {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("validate %s: %w", name, statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must point to a regular file", name)
		}
	}

	return &MMDC{
		path:                path,
		puppeteerConfigPath: config.PuppeteerConfigPath,
		mermaidConfigPath:   config.MermaidConfigPath,
		tempDir:             config.TempDir,
		maxDiagramBytes:     config.MaxDiagramBytes,
		maxOutputBytes:      config.MaxOutputBytes,
		timeout:             config.Timeout,
		slots:               make(chan struct{}, config.Concurrency),
	}, nil
}

// ParseFormat parses a format name. An empty name defaults to PNG.
func ParseFormat(value string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(value))) {
	case "", FormatPNG:
		return FormatPNG, nil
	case FormatSVG:
		return FormatSVG, nil
	default:
		return "", &Error{
			Code:    CodeUnsupportedFormat,
			Message: "format must be png or svg",
		}
	}
}

// ValidateRequest checks caller-controlled input before rendering.
func ValidateRequest(request Request, maxDiagramBytes int64) error {
	if !utf8.ValidString(request.Diagram) {
		return &Error{Code: CodeInvalidInput, Message: "diagram must be valid UTF-8"}
	}
	if strings.TrimSpace(request.Diagram) == "" {
		return &Error{Code: CodeInvalidInput, Message: "diagram must not be empty"}
	}
	if int64(len(request.Diagram)) > maxDiagramBytes {
		return &Error{
			Code:    CodeTooLarge,
			Message: fmt.Sprintf("diagram exceeds the %d-byte limit", maxDiagramBytes),
		}
	}
	if request.Format != FormatPNG && request.Format != FormatSVG {
		return &Error{Code: CodeUnsupportedFormat, Message: "format must be png or svg"}
	}
	return nil
}

// Render writes input to a private temporary directory, runs Mermaid CLI, and
// removes the directory before returning.
func (m *MMDC) Render(ctx context.Context, request Request) (result Result, resultErr error) {
	if err := ValidateRequest(request, m.maxDiagramBytes); err != nil {
		return Result{}, err
	}

	renderCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-renderCtx.Done():
		return Result{}, contextError(renderCtx.Err())
	}

	dir, err := os.MkdirTemp(m.tempDir, "mermaid-render-*")
	if err != nil {
		return Result{}, &Error{Code: CodeInternal, Message: "could not prepare rendering", Cause: err}
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			cleanupErr := &Error{Code: CodeInternal, Message: "could not remove temporary files", Cause: err}
			result = Result{}
			if resultErr == nil {
				resultErr = cleanupErr
				return
			}
			resultErr = errors.Join(cleanupErr, resultErr)
		}
	}()

	inputPath := filepath.Join(dir, "diagram.mmd")
	outputPath := filepath.Join(dir, "diagram."+string(request.Format))
	if err := os.WriteFile(inputPath, []byte(request.Diagram), 0o600); err != nil {
		return Result{}, &Error{Code: CodeInternal, Message: "could not prepare rendering", Cause: err}
	}

	args := []string{
		"--input", inputPath,
		"--output", outputPath,
		"--outputFormat", string(request.Format),
		"--quiet",
	}
	if m.mermaidConfigPath != "" {
		args = append(args, "--configFile", m.mermaidConfigPath)
	}
	if m.puppeteerConfigPath != "" {
		args = append(args, "--puppeteerConfigFile", m.puppeteerConfigPath)
	}

	cmd := exec.CommandContext(renderCtx, m.path, args...)
	configureCommand(cmd)
	stderr := &cappedWriter{limit: 4 * 1_024}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if renderCtx.Err() != nil {
			return Result{}, contextError(renderCtx.Err())
		}
		detail := sanitizeRendererMessage(stderr.String(), dir)
		message := "Mermaid could not render the diagram"
		if detail != "" {
			message += ": " + detail
		}
		return Result{}, &Error{Code: CodeRenderRejected, Message: message, Cause: err}
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		return Result{}, &Error{Code: CodeInternal, Message: "renderer produced no image", Cause: err}
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Result{}, &Error{Code: CodeInternal, Message: "renderer produced an invalid image"}
	}
	if info.Size() > m.maxOutputBytes {
		return Result{}, &Error{
			Code:    CodeTooLarge,
			Message: fmt.Sprintf("rendered image exceeds the %d-byte limit", m.maxOutputBytes),
		}
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return Result{}, &Error{Code: CodeInternal, Message: "could not read rendered image", Cause: err}
	}
	if !validImage(request.Format, data) {
		return Result{}, &Error{Code: CodeInternal, Message: "renderer produced an invalid image"}
	}

	return Result{Data: data, MIMEType: request.Format.MIMEType()}, nil
}

// MIMEType returns the media type for a format.
func (f Format) MIMEType() string {
	if f == FormatSVG {
		return "image/svg+xml"
	}
	return "image/png"
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "rendering timed out", Cause: err}
	}
	return &Error{Code: CodeCanceled, Message: "rendering was canceled", Cause: err}
}

func validImage(format Format, data []byte) bool {
	if format == FormatPNG {
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	}
	trimmed := bytes.TrimSpace(data)
	return bytes.HasPrefix(trimmed, []byte("<svg")) ||
		(bytes.HasPrefix(trimmed, []byte("<?xml")) && bytes.Contains(trimmed, []byte("<svg")))
}

func sanitizeRendererMessage(message, tempDir string) string {
	message = strings.ReplaceAll(message, tempDir, "<temp>")
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, message)

	const maxPublicMessageBytes = 1_024
	lines := strings.Split(message, "\n")
	publicLines := make([]string, 0, min(len(lines), 8))
	length := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "at ") ||
			strings.Contains(trimmed, "file://") ||
			strings.Contains(trimmed, "mermaid-cli-intercept.invalid") {
			break
		}
		if trimmed == "" {
			continue
		}
		if len(publicLines) == 8 || length+len(trimmed) > maxPublicMessageBytes {
			break
		}
		publicLines = append(publicLines, trimmed)
		length += len(trimmed)
	}
	return strings.Join(publicLines, "\n")
}

type cappedWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (w *cappedWriter) Write(data []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		_, _ = w.buffer.Write(data[:remaining])
	}
	return len(data), nil
}

func (w *cappedWriter) String() string {
	return w.buffer.String()
}
