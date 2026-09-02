// Package server exposes Mermaid rendering over HTTP and MCP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/belazy/mermaid-mcp/internal/render"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configures the HTTP and MCP handlers.
type Options struct {
	Version         string
	MaxDiagramBytes int64
	MaxInFlight     int
	Logger          *slog.Logger
}

type renderInput struct {
	Diagram string `json:"diagram" jsonschema:"Mermaid diagram source"`
	Format  string `json:"format,omitempty" jsonschema:"Output format: png or svg. Defaults to png."`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// New returns the complete HTTP handler for health, REST, and MCP routes.
func New(renderer render.Renderer, options Options) (http.Handler, error) {
	if renderer == nil {
		return nil, errors.New("renderer is required")
	}
	if options.MaxDiagramBytes <= 0 {
		return nil, errors.New("MaxDiagramBytes must be positive")
	}
	if options.MaxInFlight <= 0 {
		return nil, errors.New("MaxInFlight must be positive")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Version == "" {
		options.Version = "dev"
	}

	mcpServer := newMCPServer(renderer, options)
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			Logger:                       options.Logger,
			MaxRequestBodyBytes:          expandedJSONLimit(options.MaxDiagramBytes),
			PropagateRequestCancellation: true,
		},
	)

	limiter := newRequestLimiter(options.MaxInFlight)
	mux := http.NewServeMux()
	mux.Handle("/mcp", limiter.limit(mcpHandler))
	mux.Handle("/render", limiter.limit(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRender(writer, request, renderer, options)
	})))
	mux.HandleFunc("/healthz", handleHealth)

	originProtection := http.NewCrossOriginProtection()
	return securityHeaders(originProtection.Handler(mux)), nil
}

func newMCPServer(renderer render.Renderer, options Options) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "mermaid-renderer", Version: options.Version},
		&mcp.ServerOptions{
			Instructions: "Render Mermaid source as a PNG or SVG image. No diagrams are stored.",
			Logger:       options.Logger,
		},
	)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "render_mermaid",
		Description: "Render Mermaid diagram source and return the generated image without storing it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input renderInput) (*mcp.CallToolResult, any, error) {
		format, err := render.ParseFormat(input.Format)
		if err != nil {
			return toolError(err), nil, nil
		}
		request := render.Request{Diagram: input.Diagram, Format: format}
		if err := render.ValidateRequest(request, options.MaxDiagramBytes); err != nil {
			return toolError(err), nil, nil
		}
		result, err := renderer.Render(ctx, request)
		if err != nil {
			logRenderError(options.Logger, err)
			return toolError(err), nil, nil
		}
		data, readErr := io.ReadAll(result)
		closeErr := result.Close()
		if readErr != nil || closeErr != nil {
			err := &render.Error{
				Code:    render.CodeInternal,
				Message: "could not read rendered image",
				Cause:   errors.Join(readErr, closeErr),
			}
			logRenderError(options.Logger, err)
			return toolError(err), nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{Data: data, MIMEType: result.MIMEType},
			},
		}, nil, nil
	})
	return server
}

func handleRender(
	writer http.ResponseWriter,
	request *http.Request,
	renderer render.Renderer,
	options Options,
) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "use POST /render")
		return
	}

	input, err := parseRenderInput(writer, request, options.MaxDiagramBytes)
	if err != nil {
		writeRenderError(writer, err)
		return
	}
	result, err := renderer.Render(request.Context(), input)
	if err != nil {
		logRenderError(options.Logger, err)
		writeRenderError(writer, err)
		return
	}

	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", result.MIMEType)
	writer.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	writer.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"diagram.%s\"", input.Format))
	writer.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(writer, result)
	closeErr := result.Close()
	if copyErr != nil || closeErr != nil {
		options.Logger.Error("stream rendered image", "error", errors.Join(copyErr, closeErr))
	}
}

func parseRenderInput(writer http.ResponseWriter, request *http.Request, maxDiagramBytes int64) (render.Request, error) {
	mediaType := ""
	if header := request.Header.Get("Content-Type"); header != "" {
		parsed, _, err := mime.ParseMediaType(header)
		if err != nil {
			return render.Request{}, &render.Error{Code: render.CodeInvalidInput, Message: "invalid Content-Type header"}
		}
		mediaType = parsed
	}

	var input renderInput
	switch mediaType {
	case "", "text/plain", "application/mermaid":
		body, err := readBody(writer, request, maxDiagramBytes)
		if err != nil {
			return render.Request{}, err
		}
		input.Diagram = string(body)
	case "application/json":
		body, err := readBody(writer, request, expandedJSONLimit(maxDiagramBytes))
		if err != nil {
			return render.Request{}, err
		}
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return render.Request{}, &render.Error{Code: render.CodeInvalidInput, Message: "invalid JSON request"}
		}
		if err := ensureJSONEnd(decoder); err != nil {
			return render.Request{}, &render.Error{Code: render.CodeInvalidInput, Message: "invalid JSON request"}
		}
	default:
		return render.Request{}, &render.Error{
			Code:    render.CodeUnsupportedMediaType,
			Message: "content type must be application/json or text/plain",
		}
	}

	formatName := request.URL.Query().Get("format")
	if formatName == "" {
		formatName = input.Format
	}
	if formatName == "" && strings.Contains(request.Header.Get("Accept"), "image/svg+xml") {
		formatName = string(render.FormatSVG)
	}
	format, err := render.ParseFormat(formatName)
	if err != nil {
		return render.Request{}, err
	}
	result := render.Request{Diagram: input.Diagram, Format: format}
	if err := render.ValidateRequest(result, maxDiagramBytes); err != nil {
		return render.Request{}, err
	}
	return result, nil
}

func readBody(writer http.ResponseWriter, request *http.Request, limit int64) ([]byte, error) {
	if request.ContentLength > limit {
		return nil, &render.Error{Code: render.CodeTooLarge, Message: fmt.Sprintf("request exceeds the %d-byte limit", limit)}
	}
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	body, err := io.ReadAll(request.Body)
	if err == nil {
		return body, nil
	}
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return nil, &render.Error{Code: render.CodeTooLarge, Message: fmt.Sprintf("request exceeds the %d-byte limit", limit)}
	}
	return nil, &render.Error{Code: render.CodeInvalidInput, Message: "could not read request body", Cause: err}
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON request contains trailing data")
	}
	return nil
}

func expandedJSONLimit(diagramLimit int64) int64 {
	const overhead int64 = 64 * 1_024
	if diagramLimit > (math.MaxInt64-overhead)/6 {
		return math.MaxInt64
	}
	return diagramLimit*6 + overhead
}

func handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "use GET /healthz")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ok"})
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: render.PublicMessage(err)}},
		IsError: true,
	}
}

func writeRenderError(writer http.ResponseWriter, err error) {
	var renderErr *render.Error
	if !errors.As(err, &renderErr) {
		writeError(writer, http.StatusInternalServerError, string(render.CodeInternal), "rendering failed")
		return
	}
	status := http.StatusInternalServerError
	switch renderErr.Code {
	case render.CodeInvalidInput, render.CodeUnsupportedFormat:
		status = http.StatusBadRequest
	case render.CodeUnsupportedMediaType:
		status = http.StatusUnsupportedMediaType
	case render.CodeTooLarge:
		status = http.StatusRequestEntityTooLarge
	case render.CodeRenderRejected:
		status = http.StatusUnprocessableEntity
	case render.CodeTimeout:
		status = http.StatusGatewayTimeout
	case render.CodeCanceled:
		status = http.StatusRequestTimeout
	case render.CodeInternal:
		status = http.StatusInternalServerError
	}
	writeError(writer, status, string(renderErr.Code), render.PublicMessage(err))
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(errorResponse{Error: apiError{Code: code, Message: message}})
}

func logRenderError(logger *slog.Logger, err error) {
	var renderErr *render.Error
	if errors.As(err, &renderErr) && renderErr.Code == render.CodeRenderRejected {
		logger.Info("diagram rejected", "code", renderErr.Code)
		return
	}
	logger.Error("render failed", "error", err)
}

type requestLimiter struct {
	slots chan struct{}
}

func newRequestLimiter(max int) *requestLimiter {
	return &requestLimiter{slots: make(chan struct{}, max)}
}

func (l *requestLimiter) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case l.slots <- struct{}{}:
			defer func() { <-l.slots }()
			next.ServeHTTP(writer, request)
		default:
			writer.Header().Set("Retry-After", "1")
			writeError(writer, http.StatusServiceUnavailable, "overloaded", "server is busy; retry later")
		}
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}
