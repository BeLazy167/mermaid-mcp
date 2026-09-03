// Package server exposes Mermaid rendering over HTTP and MCP.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/belazy/mermaid-mcp/internal/assetstore"
	"github.com/belazy/mermaid-mcp/internal/render"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configures the HTTP and MCP handlers.
type Options struct {
	Version          string
	RendererBundleID string
	MaxDiagramBytes  int64
	MaxInFlight      int
	ClientIPHeader   string
	Ready            func() bool
	CacheStats       func() render.CacheStats
	AssetStore       AssetStore
	AssetHMACKey     []byte
	Clock            func() time.Time
	Logger           *slog.Logger
}

// AssetStore publishes immutable, content-addressed render outputs.
type AssetStore interface {
	Lookup(context.Context, string) (assetstore.Metadata, bool, error)
	Publish(context.Context, string, string, []byte) (assetstore.Metadata, error)
}

type renderInput struct {
	Diagram  string `json:"diagram" jsonschema:"Mermaid diagram source"`
	Format   string `json:"format,omitempty" jsonschema:"Output format: png or svg. Defaults to png."`
	Delivery string `json:"delivery,omitempty" jsonschema:"Delivery mode: inline or url. Defaults to inline."`
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
	if options.RendererBundleID == "" {
		options.RendererBundleID = "dev"
	}
	if options.AssetStore != nil && len(options.AssetHMACKey) < 32 {
		return nil, errors.New("AssetHMACKey must contain at least 32 bytes when AssetStore is configured")
	}
	if options.Clock == nil {
		options.Clock = time.Now
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
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, request *http.Request) {
		handleReadiness(writer, request, options.Ready)
	})
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		handleMetrics(writer, request, options)
	})

	originProtection := http.NewCrossOriginProtection()
	return securityHeaders(originProtection.Handler(withClientIdentity(mux, options.ClientIPHeader))), nil
}

func withClientIdentity(next http.Handler, trustedHeader string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity := ""
		if trustedHeader != "" {
			identity = strings.TrimSpace(request.Header.Get(trustedHeader))
		}
		if identity == "" {
			identity = request.RemoteAddr
			if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
				identity = host
			}
		}
		ctx := render.WithClientIdentity(request.Context(), identity)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func newMCPServer(renderer render.Renderer, options Options) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "mermaid-renderer", Version: options.Version},
		&mcp.ServerOptions{
			Instructions: "Render Mermaid source as PNG or SVG. Source is never stored; URL delivery may store immutable image output.",
			Logger:       options.Logger,
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		},
	)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "render_mermaid",
		Description: "Render Mermaid source. Return inline image bytes or an optional immutable asset URL.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input renderInput) (*mcp.CallToolResult, any, error) {
		format, err := render.ParseFormat(input.Format)
		if err != nil {
			return toolError(err), nil, nil
		}
		request := render.Request{Diagram: input.Diagram, Format: format}
		if err := render.ValidateRequest(request, options.MaxDiagramBytes); err != nil {
			return toolError(err), nil, nil
		}
		delivery := strings.ToLower(strings.TrimSpace(input.Delivery))
		if delivery == "" {
			delivery = "inline"
		}
		if delivery != "inline" && delivery != "url" {
			return toolError(&render.Error{Code: render.CodeInvalidInput, Message: "delivery must be inline or url"}), nil, nil
		}
		if delivery == "url" && options.AssetStore == nil {
			return toolError(&render.Error{Code: render.CodeInvalidInput, Message: "URL delivery is not configured"}), nil, nil
		}
		releaseRequestAdmission(ctx)

		var objectKey string
		var expiresAt time.Time
		if delivery == "url" {
			contentKey := render.ContentKey(options.RendererBundleID, request)
			now := options.Clock().UTC()
			objectKey = assetObjectKey(options.AssetHMACKey, contentKey, request.Format, now)
			expiresAt = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			metadata, found, lookupErr := options.AssetStore.Lookup(ctx, objectKey)
			if lookupErr != nil {
				err := &render.Error{Code: render.CodeInternal, Message: "could not check rendered asset", Cause: lookupErr}
				logRenderError(options.Logger, err)
				return toolError(err), nil, nil
			}
			if found {
				return assetToolResult(metadata, request.Format, expiresAt), nil, nil
			}
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
		if delivery == "inline" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.ImageContent{Data: data, MIMEType: result.MIMEType},
				},
			}, nil, nil
		}

		metadata, err := options.AssetStore.Publish(ctx, objectKey, result.MIMEType, data)
		if err != nil {
			renderErr := &render.Error{Code: render.CodeInternal, Message: "could not publish rendered image", Cause: err}
			logRenderError(options.Logger, renderErr)
			return toolError(renderErr), nil, nil
		}
		return assetToolResult(metadata, request.Format, expiresAt), nil, nil
	})
	return server
}

func assetObjectKey(hmacKey []byte, contentKey string, format render.Format, now time.Time) string {
	day := now.UTC().Format("2006-01-02")
	digest := hmac.New(sha256.New, hmacKey)
	_, _ = io.WriteString(digest, day)
	_, _ = digest.Write([]byte{0})
	_, _ = io.WriteString(digest, contentKey)
	token := base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
	return "assets/v1/" + day + "/" + token + "." + string(format)
}

func assetToolResult(metadata assetstore.Metadata, format render.Format, expiresAt time.Time) *mcp.CallToolResult {
	size := metadata.Size
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.ResourceLink{
			URI:         metadata.URL,
			Name:        "diagram." + string(format),
			Description: "Immutable rendered Mermaid image",
			MIMEType:    metadata.MIMEType,
			Size:        &size,
		}},
		StructuredContent: map[string]any{
			"delivery":  "url",
			"url":       metadata.URL,
			"mimeType":  metadata.MIMEType,
			"size":      size,
			"sha256":    metadata.SHA256,
			"expiresAt": expiresAt.Format(time.RFC3339),
		},
	}
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
	releaseRequestAdmission(request.Context())
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
	contentEncoding := strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Encoding")))
	if contentEncoding != "" && contentEncoding != "identity" {
		return render.Request{}, &render.Error{Code: render.CodeUnsupportedMediaType, Message: "compressed request bodies are not supported"}
	}
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

	if input.Delivery != "" {
		return render.Request{}, &render.Error{Code: render.CodeInvalidInput, Message: "delivery is available only through MCP"}
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

func handleMetrics(writer http.ResponseWriter, request *http.Request, options Options) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "use GET /metrics")
		return
	}
	stats := render.CacheStats{}
	if options.CacheStats != nil {
		stats = options.CacheStats()
	}
	ready := 1
	if options.Ready != nil && !options.Ready() {
		ready = 0
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, `# TYPE mermaid_cache_hits_total counter
mermaid_cache_hits_total %d
# TYPE mermaid_cache_misses_total counter
mermaid_cache_misses_total %d
# TYPE mermaid_cache_coalesced_total counter
mermaid_cache_coalesced_total %d
# TYPE mermaid_render_overloads_total counter
mermaid_render_overloads_total %d
# TYPE mermaid_render_capacity_overloads_total counter
mermaid_render_capacity_overloads_total %d
# TYPE mermaid_render_rate_overloads_total counter
mermaid_render_rate_overloads_total %d
# TYPE mermaid_render_cluster_overloads_total counter
mermaid_render_cluster_overloads_total %d
# TYPE mermaid_cache_evictions_total counter
mermaid_cache_evictions_total %d
# TYPE mermaid_cache_expirations_total counter
mermaid_cache_expirations_total %d
# TYPE mermaid_render_fills_total counter
mermaid_render_fills_total %d
# TYPE mermaid_cache_bytes gauge
mermaid_cache_bytes %d
# TYPE mermaid_cache_entries gauge
mermaid_cache_entries %d
# TYPE mermaid_render_pending_fills gauge
mermaid_render_pending_fills %d
# TYPE mermaid_render_active_fills gauge
mermaid_render_active_fills %d
# TYPE mermaid_renderer_ready gauge
mermaid_renderer_ready %d
`, stats.Hits, stats.Misses, stats.Coalesced, stats.Overloads, stats.CapacityOverloads,
		stats.RateOverloads, stats.ClusterOverloads, stats.Evictions, stats.Expirations, stats.Fills, stats.Bytes, stats.Entries,
		stats.PendingFills, stats.ActiveFills, ready)
}

func handleReadiness(writer http.ResponseWriter, request *http.Request, ready func() bool) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "use GET /readyz")
		return
	}
	if ready != nil && !ready() {
		writeError(writer, http.StatusServiceUnavailable, "not_ready", "renderer is not ready")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready"})
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
	case render.CodeOverloaded:
		status = http.StatusTooManyRequests
		writer.Header().Set("Retry-After", "1")
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
	if errors.As(err, &renderErr) {
		switch renderErr.Code {
		case render.CodeInvalidInput, render.CodeRenderRejected, render.CodeCanceled, render.CodeOverloaded:
			logger.Debug("render request rejected", "code", renderErr.Code)
			return
		case render.CodeTimeout:
			logger.Warn("render timed out", "code", renderErr.Code)
			return
		}
	}
	logger.Error("render failed", "error", err)
}

type requestLimiter struct {
	slots chan struct{}
}

type requestAdmission struct {
	once    sync.Once
	release func()
}

type requestAdmissionKey struct{}

func newRequestLimiter(max int) *requestLimiter {
	return &requestLimiter{slots: make(chan struct{}, max)}
}

func (l *requestLimiter) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case l.slots <- struct{}{}:
			admission := &requestAdmission{release: func() { <-l.slots }}
			defer admission.once.Do(admission.release)
			ctx := context.WithValue(request.Context(), requestAdmissionKey{}, admission)
			next.ServeHTTP(writer, request.WithContext(ctx))
		default:
			writer.Header().Set("Retry-After", "1")
			writeError(writer, http.StatusServiceUnavailable, "overloaded", "server is busy; retry later")
		}
	})
}

func releaseRequestAdmission(ctx context.Context) {
	if admission, ok := ctx.Value(requestAdmissionKey{}).(*requestAdmission); ok {
		admission.once.Do(admission.release)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}
