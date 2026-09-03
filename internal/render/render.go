// Package render converts Mermaid source into image bytes.
package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
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

var (
	mutableDirectivePattern = regexp.MustCompile(`(?is)%%\s*\{\s*(?:init|config)\s*:`)
	frontMatterPattern      = regexp.MustCompile(`(?s)^\s*---[ \t]*\r?\n(.*?)\r?\n---(?:[ \t]*\r?\n|[ \t]*$)`)
	frontMatterConfig       = regexp.MustCompile(`(?m)^[ \t]*config[ \t]*:`)
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
	// CodeOverloaded means the bounded render-miss budget is full.
	CodeOverloaded ErrorCode = "overloaded"
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

// Result streams a rendered image. The caller must close it to release resources.
type Result struct {
	io.ReadCloser
	MIMEType string
	Size     int64
}

// Renderer renders Mermaid diagrams.
type Renderer interface {
	Render(context.Context, Request) (Result, error)
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
	if mutableDirectivePattern.MatchString(request.Diagram) || hasFrontMatterConfig(request.Diagram) {
		return &Error{Code: CodeRenderRejected, Message: "diagram configuration directives are not allowed"}
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

// MIMEType returns the media type for a format.
func (f Format) MIMEType() string {
	if f == FormatSVG {
		return "image/svg+xml"
	}
	return "image/png"
}

func hasFrontMatterConfig(diagram string) bool {
	match := frontMatterPattern.FindStringSubmatch(diagram)
	return len(match) == 2 && frontMatterConfig.MatchString(match[1])
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "rendering timed out", Cause: err}
	}
	return &Error{Code: CodeCanceled, Message: "rendering was canceled", Cause: err}
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
