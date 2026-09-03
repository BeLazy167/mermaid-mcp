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

	"github.com/belazy/mermaid-mcp/internal/assetstore"
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
			name:        "rejects REST delivery mode",
			contentType: "application/json",
			body:        `{"diagram":"graph TD; A-->B","delivery":"url"}`,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "delivery is available only through MCP",
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
		{
			name:        "maps render-miss overload",
			contentType: "text/plain",
			body:        "graph TD; A-->B",
			renderErr: &render.Error{
				Code:    render.CodeOverloaded,
				Message: "render capacity is full",
			},
			wantStatus: http.StatusTooManyRequests,
			wantBody:   "render capacity is full",
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
			if tt.wantStatus == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "" {
				t.Fatal("missing Retry-After header")
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
		{name: "asset store without HMAC key", renderer: &fakeRenderer{}, options: Options{MaxDiagramBytes: 1, MaxInFlight: 1, AssetStore: &fakeAssetStore{}}},
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
		{name: "readiness requires get", method: http.MethodPost, path: "/readyz", wantStatus: http.StatusMethodNotAllowed},
		{name: "metrics requires get", method: http.MethodPost, path: "/metrics", wantStatus: http.StatusMethodNotAllowed},
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

func TestRenderEndpointRejectsCompressedBody(t *testing.T) {
	handler := mustNew(t, &fakeRenderer{}, testOptions())
	request := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader("compressed"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnsupportedMediaType)
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

func TestSlowRenderDoesNotHoldRequestAdmission(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRenderer{started: started, release: release}
	options := testOptions()
	options.MaxInFlight = 1
	handler := mustNew(t, fake, options)

	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, diagram := range []string{"graph TD; A-->B", "graph TD; B-->C"} {
		go func() {
			request := httptest.NewRequest(http.MethodPost, "/render", strings.NewReader(diagram))
			request.Header.Set("Content-Type", "text/plain")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses <- response
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("request did not reach renderer")
		}
	}

	close(release)
	for range 2 {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not finish")
		}
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
	initialize := session.InitializeResult()
	if initialize == nil || initialize.Capabilities.Tools == nil || initialize.Capabilities.Tools.ListChanged {
		t.Fatalf("server capabilities = %#v", initialize)
	}
	capabilitiesJSON, err := json.Marshal(initialize.Capabilities)
	if err != nil {
		t.Fatalf("marshal server capabilities: %v", err)
	}
	if bytes.Contains(capabilitiesJSON, []byte(`"logging"`)) {
		t.Fatalf("server capabilities include deprecated logging: %s", capabilitiesJSON)
	}

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

func TestAssetObjectKeyIsOpaqueAndDayScoped(t *testing.T) {
	contentKey := strings.Repeat("a", 64)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	first := assetObjectKey([]byte(strings.Repeat("k", 32)), contentKey, render.FormatPNG, now)
	if !strings.HasPrefix(first, "assets/v1/2026-09-03/") || !strings.HasSuffix(first, ".png") || strings.Contains(first, contentKey) {
		t.Fatalf("assetObjectKey() = %q", first)
	}
	if first != assetObjectKey([]byte(strings.Repeat("k", 32)), contentKey, render.FormatPNG, now) {
		t.Fatal("assetObjectKey() is not deterministic")
	}
	if first == assetObjectKey([]byte(strings.Repeat("k", 32)), contentKey, render.FormatPNG, now.Add(24*time.Hour)) {
		t.Fatal("assetObjectKey() did not change by day")
	}
	if first == assetObjectKey([]byte(strings.Repeat("x", 32)), contentKey, render.FormatPNG, now) {
		t.Fatal("assetObjectKey() did not change by secret")
	}
}

func TestMCPRenderToolURLDelivery(t *testing.T) {
	store := &fakeAssetStore{metadata: assetstore.Metadata{
		URL: "https://assets.example.com/v1/render.svg", MIMEType: "image/svg+xml",
		SHA256: strings.Repeat("a", 64), Size: 8,
	}}
	options := testOptions()
	options.AssetStore = store
	options.AssetHMACKey = []byte(strings.Repeat("k", 32))
	options.Clock = func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	httpServer := httptest.NewServer(mustNew(t, &fakeRenderer{}, options))
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "render_mermaid",
		Arguments: map[string]any{
			"diagram":  "graph TD; A-->B",
			"format":   "svg",
			"delivery": "url",
		},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("result = %#v", result)
	}
	link, ok := result.Content[0].(*mcp.ResourceLink)
	if !ok || link.URI != store.metadata.URL || link.MIMEType != "image/svg+xml" || link.Size == nil || *link.Size != 8 {
		t.Fatalf("link = %#v", result.Content[0])
	}
	if store.mimeType != "image/svg+xml" || string(store.data) != "svg-data" {
		t.Fatalf("store call = %q %q", store.mimeType, store.data)
	}
	if !strings.HasPrefix(store.key, "assets/v1/2026-09-03/") || !strings.HasSuffix(store.key, ".svg") {
		t.Fatalf("store key = %q", store.key)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["url"] != store.metadata.URL || structured["delivery"] != "url" {
		t.Fatalf("structured content = %#v", result.StructuredContent)
	}
}

func TestMCPURLDeliverySkipsRenderOnSharedStoreHit(t *testing.T) {
	store := &fakeAssetStore{
		found: true,
		metadata: assetstore.Metadata{
			URL: "https://assets.example.com/existing.png", MIMEType: "image/png",
			SHA256: strings.Repeat("b", 64), Size: 123,
		},
	}
	renderer := &fakeRenderer{}
	options := testOptions()
	options.AssetStore = store
	options.AssetHMACKey = []byte(strings.Repeat("k", 32))
	httpServer := httptest.NewServer(mustNew(t, renderer, options))
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "render_mermaid",
		Arguments: map[string]any{"diagram": "graph TD; A-->B", "delivery": "url"},
	})
	if err != nil || result.IsError {
		t.Fatalf("CallTool() result = %#v, error = %v", result, err)
	}
	if renderer.renderCount() != 0 || store.lookupCalls != 1 || store.publishCalls != 0 {
		t.Fatalf("calls: render=%d lookup=%d publish=%d", renderer.renderCount(), store.lookupCalls, store.publishCalls)
	}
}

func TestMCPRenderToolRejectsUnavailableURLDelivery(t *testing.T) {
	httpServer := httptest.NewServer(mustNew(t, &fakeRenderer{}, testOptions()))
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "render_mermaid",
		Arguments: map[string]any{"diagram": "graph TD; A-->B", "delivery": "url"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result = %#v, want tool error", result)
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

func TestReadinessEndpoint(t *testing.T) {
	ready := false
	options := testOptions()
	options.Ready = func() bool { return ready }
	handler := mustNew(t, &fakeRenderer{}, options)

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}

	ready = true
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	options := testOptions()
	options.Ready = func() bool { return false }
	options.CacheStats = func() render.CacheStats {
		return render.CacheStats{Hits: 7, Misses: 3, Coalesced: 2, Overloads: 1, ClusterOverloads: 1, Bytes: 99, Entries: 4}
	}
	handler := mustNew(t, &fakeRenderer{}, options)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	for _, line := range []string{
		"mermaid_cache_hits_total 7",
		"mermaid_cache_misses_total 3",
		"mermaid_cache_coalesced_total 2",
		"mermaid_render_overloads_total 1",
		"mermaid_render_cluster_overloads_total 1",
		"mermaid_cache_bytes 99",
		"mermaid_renderer_ready 0",
	} {
		if !strings.Contains(response.Body.String(), line) {
			t.Fatalf("metrics missing %q:\n%s", line, response.Body.String())
		}
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

type fakeAssetStore struct {
	key          string
	mimeType     string
	data         []byte
	metadata     assetstore.Metadata
	found        bool
	lookupErr    error
	publishErr   error
	lookupCalls  int
	publishCalls int
}

func (s *fakeAssetStore) Lookup(_ context.Context, key string) (assetstore.Metadata, bool, error) {
	s.key = key
	s.lookupCalls++
	return s.metadata, s.found, s.lookupErr
}

func (s *fakeAssetStore) Publish(_ context.Context, key, mimeType string, data []byte) (assetstore.Metadata, error) {
	s.key = key
	s.mimeType = mimeType
	s.data = append([]byte(nil), data...)
	s.publishCalls++
	return s.metadata, s.publishErr
}

func TestTrustedClientIdentityRateLimitsUniqueMissesOnly(t *testing.T) {
	cache, err := render.NewCache(validSVGRenderer{}, render.CacheConfig{
		BundleID: "bundle-v1", MaxBytes: 1 << 20, MaxEntries: 100,
		MaxOutputBytes: 1 << 20, TTL: time.Minute, RejectionTTL: time.Second,
		FillTimeout: time.Second, MaxWaiters: 10, MaxConcurrentFills: 1, MaxQueuedFills: 1,
		MissesPerSecond: 100, MissBurst: 100,
		ClientMissesPerSecond: 1, ClientMissBurst: 1, MaxClients: 10,
	})
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	options := testOptions()
	options.ClientIPHeader = "X-Test-Client-IP"
	handler := mustNew(t, cache, options)
	request := func(client, diagram string) int {
		req := httptest.NewRequest(http.MethodPost, "/render?format=svg", strings.NewReader(diagram))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("X-Test-Client-IP", client)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Code
	}

	if got := request("client-a", "graph TD; A-->B"); got != http.StatusOK {
		t.Fatalf("first status = %d", got)
	}
	if got := request("client-a", "graph TD; B-->C"); got != http.StatusTooManyRequests {
		t.Fatalf("same-client unique status = %d", got)
	}
	if got := request("client-a", "graph TD; A-->B"); got != http.StatusOK {
		t.Fatalf("same-client hit status = %d", got)
	}
	if got := request("client-b", "graph TD; B-->C"); got != http.StatusOK {
		t.Fatalf("other-client status = %d", got)
	}
}

type validSVGRenderer struct{}

func (validSVGRenderer) Render(_ context.Context, _ render.Request) (render.Result, error) {
	data := `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`
	return render.Result{ReadCloser: io.NopCloser(strings.NewReader(data)), MIMEType: "image/svg+xml", Size: int64(len(data))}, nil
}

type fakeRenderer struct {
	mu      sync.Mutex
	request render.Request
	err     error
	calls   int
	closed  int
	started chan struct{}
	release chan struct{}
}

func (f *fakeRenderer) Render(ctx context.Context, request render.Request) (render.Result, error) {
	f.mu.Lock()
	f.request = request
	f.calls++
	f.mu.Unlock()
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		case <-ctx.Done():
			return render.Result{}, ctx.Err()
		}
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

func (f *fakeRenderer) renderCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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
		Version:          "test",
		RendererBundleID: "bundle-v1",
		MaxDiagramBytes:  1_024,
		MaxInFlight:      4,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
