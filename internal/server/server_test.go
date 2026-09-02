package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/belazy/mermaid-mcp/internal/render"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRenderEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		accept      string
		query       string
		body        string
		renderErr   error
		wantStatus  int
		wantFormat  render.Format
		wantBody    string
		wantMIME    string
	}{
		{
			name:        "plain text defaults to png",
			contentType: "text/plain; charset=utf-8",
			body:        "graph TD; A-->B",
			wantStatus:  http.StatusOK,
			wantFormat:  render.FormatPNG,
			wantBody:    "png-data",
			wantMIME:    "image/png",
		},
		{
			name:        "plain text negotiates svg",
			contentType: "text/plain",
			accept:      "image/svg+xml",
			body:        "graph TD; A-->B",
			wantStatus:  http.StatusOK,
			wantFormat:  render.FormatSVG,
			wantBody:    "svg-data",
			wantMIME:    "image/svg+xml",
		},
		{
			name:        "json selects svg",
			contentType: "application/json",
			body:        `{"diagram":"sequenceDiagram\nA->>B: Hi","format":"svg"}`,
			wantStatus:  http.StatusOK,
			wantFormat:  render.FormatSVG,
			wantBody:    "svg-data",
			wantMIME:    "image/svg+xml",
		},
		{
			name:        "query selects svg",
			contentType: "text/plain",
			query:       "?format=svg",
			body:        "graph TD; A-->B",
			wantStatus:  http.StatusOK,
			wantFormat:  render.FormatSVG,
			wantBody:    "svg-data",
			wantMIME:    "image/svg+xml",
		},
		{
			name:        "rejects unknown json field",
			contentType: "application/json",
			body:        `{"diagram":"graph TD; A-->B","unknown":true}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "invalid JSON request",
		},
		{
			name:        "rejects unsupported media type",
			contentType: "application/xml",
			body:        "<diagram />",
			wantStatus:  http.StatusUnsupportedMediaType,
			wantBody:    "content type must be application/json or text/plain",
		},
		{
			name:        "maps renderer rejection",
			contentType: "text/plain",
			body:        "bad",
			renderErr: &render.Error{
				Code:    render.CodeRenderRejected,
				Message: "Mermaid rejected the diagram",
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantBody:   "Mermaid rejected the diagram",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRenderer{err: tt.renderErr}
			handler := mustNew(t, fake, testOptions())
			req := httptest.NewRequest(http.MethodPost, "/render"+tt.query, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, req)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", response.Body.String(), tt.wantBody)
			}
			if tt.wantMIME != "" && response.Header().Get("Content-Type") != tt.wantMIME {
				t.Fatalf("Content-Type = %q, want %q", response.Header().Get("Content-Type"), tt.wantMIME)
			}
			if response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing X-Content-Type-Options header")
			}
			if tt.wantFormat != "" && fake.lastRequest().Format != tt.wantFormat {
				t.Fatalf("renderer format = %q, want %q", fake.lastRequest().Format, tt.wantFormat)
			}
			if tt.wantMIME != "" && fake.closeCount() != 1 {
				t.Fatalf("result close count = %d, want 1", fake.closeCount())
			}
		})
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name     string
		renderer render.Renderer
		options  Options
	}{
		{name: "missing renderer", renderer: nil, options: testOptions()},
		{name: "invalid diagram limit", renderer: &fakeRenderer{}, options: Options{MaxInFlight: 1}},
		{name: "invalid in-flight limit", renderer: &fakeRenderer{}, options: Options{MaxDiagramBytes: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.renderer, tt.options); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}

func TestMethods(t *testing.T) {
	options := testOptions()
	options.Logger = nil
	options.Version = ""
	handler := mustNew(t, &fakeRenderer{}, options)
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "render requires post", method: http.MethodGet, path: "/render", wantStatus: http.StatusMethodNotAllowed},
		{name: "health requires get", method: http.MethodPost, path: "/healthz", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
		})
	}
}

func TestRenderEndpointRejectsUnsupportedFormat(t *testing.T) {
	handler := mustNew(t, &fakeRenderer{}, testOptions())
	request := httptest.NewRequest(http.MethodPost, "/render?format=pdf", strings.NewReader("graph TD; A-->B"))
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestRenderEndpointRejectsOversizedBody(t *testing.T) {
	options := testOptions()
	options.MaxDiagramBytes = 4
	handler := mustNew(t, &fakeRenderer{}, options)
	request := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("12345"))
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
}

func TestRenderEndpointBoundsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRenderer{started: started, release: release}
	options := testOptions()
	options.MaxInFlight = 1
	handler := mustNew(t, fake, options)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("graph TD; A-->B"))
		request.Header.Set("Content-Type", "text/plain")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		firstDone <- response
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach renderer")
	}

	request := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("graph TD; B-->C"))
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish")
	}
}

func TestMCPRenderTool(t *testing.T) {
	fake := &fakeRenderer{}
	httpServer := httptest.NewServer(mustNew(t, fake, testOptions()))
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: httpServer.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	}()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "render_mermaid" {
		t.Fatalf("tools = %#v, want render_mermaid", tools.Tools)
	}

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "render_mermaid",
		Arguments: map[string]any{
			"diagram": "graph TD; A-->B",
			"format":  "svg",
		},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %#v", result.Content)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content count = %d, want 1", len(result.Content))
	}
	image, ok := result.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.ImageContent", result.Content[0])
	}
	if image.MIMEType != "image/svg+xml" || !bytes.Equal(image.Data, []byte("svg-data")) {
		t.Fatalf("image = %#v", image)
	}
	if fake.closeCount() != 1 {
		t.Fatalf("result close count = %d, want 1", fake.closeCount())
	}

	invalid, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "render_mermaid",
		Arguments: map[string]any{
			"diagram": "graph TD; A-->B",
			"format":  "pdf",
		},
	})
	if err != nil {
		t.Fatalf("call invalid tool input: %v", err)
	}
	if !invalid.IsError || len(invalid.Content) != 1 {
		t.Fatalf("invalid tool result = %#v", invalid)
	}
	text, ok := invalid.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "png or svg") {
		t.Fatalf("invalid tool content = %#v", invalid.Content)
	}
}

func TestCrossOriginRenderRequestIsRejected(t *testing.T) {
	handler := mustNew(t, &fakeRenderer{}, testOptions())
	request := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("graph TD; A-->B"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Origin", "https://attacker.example")
	request.Host = "renderer.example"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestHealthEndpoint(t *testing.T) {
	handler := mustNew(t, &fakeRenderer{}, testOptions())
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status body = %#v", body)
	}
}

type fakeRenderer struct {
	mu      sync.Mutex
	request render.Request
	err     error
	closed  int
	started chan struct{}
	release chan struct{}
}

func (f *fakeRenderer) Render(ctx context.Context, request render.Request) (render.Result, error) {
	f.mu.Lock()
	f.request = request
	f.mu.Unlock()
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return render.Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return render.Result{}, f.err
	}
	if request.Format == render.FormatSVG {
		return f.result("svg-data", "image/svg+xml"), nil
	}
	return f.result("png-data", "image/png"), nil
}

func (f *fakeRenderer) result(data, mimeType string) render.Result {
	return render.Result{
		ReadCloser: &trackingReadCloser{
			Reader: strings.NewReader(data),
			close: func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.closed++
			},
		},
		MIMEType: mimeType,
		Size:     int64(len(data)),
	}
}

func (f *fakeRenderer) lastRequest() render.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.request
}

func (f *fakeRenderer) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type trackingReadCloser struct {
	io.Reader
	close func()
}

func (r *trackingReadCloser) Close() error {
	r.close()
	return nil
}

func mustNew(t *testing.T, renderer render.Renderer, options Options) http.Handler {
	t.Helper()
	handler, err := New(renderer, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return handler
}

func testOptions() Options {
	return Options{
		Version:         "test",
		MaxDiagramBytes: 1_024,
		MaxInFlight:     4,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
