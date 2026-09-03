package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecuteBoundsConcurrency(t *testing.T) {
	const concurrency = 4
	started := make(chan struct{}, concurrency)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	var mu sync.Mutex
	active := 0
	maximum := 0
	requestError := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			mu.Lock()
			requestError = err.Error()
			mu.Unlock()
		}
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		if request.Method != http.MethodPost || request.URL.Path != "/render" || request.URL.Query().Get("format") != "png" {
			requestError = fmt.Sprintf("unexpected request: %s %s", request.Method, request.URL.String())
		}
		if request.Header.Get("Content-Type") != "text/plain" || request.Header.Get("Accept") != "image/png" {
			requestError = fmt.Sprintf("unexpected headers: %#v", request.Header)
		}
		if string(body) != defaultDiagram {
			requestError = fmt.Sprintf("unexpected body: %q", body)
		}
		mu.Unlock()

		select {
		case started <- struct{}{}:
		default:
		}
		<-release

		writer.Header().Set("Content-Type", "image/png")
		_, _ = writer.Write([]byte("image"))
		mu.Lock()
		active--
		mu.Unlock()
	}))
	defer server.Close()

	cfg := validTestConfig(t, server.URL, 12, concurrency, false, "png")
	done := make(chan report, 1)
	go func() {
		done <- execute(context.Background(), server.Client(), cfg)
	}()

	for range concurrency {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("requests did not reach configured concurrency")
		}
	}
	releaseOnce.Do(func() { close(release) })

	var result report
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("load test did not finish")
	}

	mu.Lock()
	gotMaximum := maximum
	gotRequestError := requestError
	mu.Unlock()
	if gotRequestError != "" {
		t.Fatal(gotRequestError)
	}
	if gotMaximum != concurrency {
		t.Fatalf("maximum concurrency = %d, want %d", gotMaximum, concurrency)
	}
	if result.total != cfg.total || result.success != cfg.total || result.errors != 0 {
		t.Fatalf("report = %#v", result)
	}
	if result.bytes != int64(cfg.total*len("image")) {
		t.Fatalf("bytes = %d, want %d", result.bytes, cfg.total*len("image"))
	}
	if result.statusCodes[http.StatusOK] != cfg.total {
		t.Fatalf("status codes = %#v", result.statusCodes)
	}
}

func TestExecuteRepeatedAndUniqueInputs(t *testing.T) {
	tests := []struct {
		name       string
		unique     bool
		wantUnique int
	}{
		{name: "repeated cache-hit workload", unique: false, wantUnique: 1},
		{name: "unique valid Mermaid comments", unique: true, wantUnique: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			var requestError string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					mu.Lock()
					requestError = err.Error()
					mu.Unlock()
				}
				mu.Lock()
				bodies = append(bodies, string(body))
				if request.URL.Query().Get("format") != "svg" || request.Header.Get("Accept") != "image/svg+xml" {
					requestError = "SVG format was not requested"
				}
				mu.Unlock()
				writer.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
				_, _ = writer.Write([]byte("<svg/>"))
			}))
			defer server.Close()

			cfg := validTestConfig(t, server.URL+"/render", 8, 3, tt.unique, "svg")
			result := execute(context.Background(), server.Client(), cfg)
			if result.success != cfg.total || result.errors != 0 {
				t.Fatalf("report = %#v", result)
			}

			mu.Lock()
			gotBodies := append([]string(nil), bodies...)
			gotRequestError := requestError
			mu.Unlock()
			if gotRequestError != "" {
				t.Fatal(gotRequestError)
			}
			if len(gotBodies) != cfg.total {
				t.Fatalf("body count = %d, want %d", len(gotBodies), cfg.total)
			}

			uniqueBodies := make(map[string]struct{}, len(gotBodies))
			for _, body := range gotBodies {
				uniqueBodies[body] = struct{}{}
				if !strings.HasPrefix(body, defaultDiagram) {
					t.Fatalf("body does not preserve diagram: %q", body)
				}
				if tt.unique && !strings.Contains(body, "\n%% loadtest request ") {
					t.Fatalf("unique body lacks Mermaid comment: %q", body)
				}
			}
			if len(uniqueBodies) != tt.wantUnique {
				t.Fatalf("unique body count = %d, want %d", len(uniqueBodies), tt.wantUnique)
			}
		})
	}
}

func TestLatencyPercentilesUseNearestRank(t *testing.T) {
	samples := []time.Duration{
		40 * time.Millisecond,
		10 * time.Millisecond,
		100 * time.Millisecond,
		20 * time.Millisecond,
	}

	p50, p95, p99, maximum := latencyPercentiles(samples)
	if p50 != 20*time.Millisecond {
		t.Fatalf("p50 = %s, want 20ms", p50)
	}
	if p95 != 100*time.Millisecond {
		t.Fatalf("p95 = %s, want 100ms", p95)
	}
	if p99 != 100*time.Millisecond {
		t.Fatalf("p99 = %s, want 100ms", p99)
	}
	if maximum != 100*time.Millisecond {
		t.Fatalf("max = %s, want 100ms", maximum)
	}
	if samples[0] != 40*time.Millisecond {
		t.Fatal("latencyPercentiles mutated its input")
	}
}

func TestCLIExitsNonzeroOnMIMEMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/jpeg")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("not png"))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli(context.Background(), []string{
		"-target", server.URL,
		"-requests", "3",
		"-concurrency", "2",
		"-timeout", "1s",
	}, &stdout, &stderr)

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	var output jsonReport
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode report: %v; output = %q", err, stdout.String())
	}
	if output.Total != 3 || output.Success != 0 || output.Errors != 3 {
		t.Fatalf("report = %#v", output)
	}
	if output.Bytes != int64(3*len("not png")) {
		t.Fatalf("bytes = %d", output.Bytes)
	}
	if !strings.Contains(stderr.String(), "3 of 3 requests failed") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCLIExitsNonzeroOnHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("busy"))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := cli(context.Background(), []string{
		"-target", server.URL,
		"-requests", "1",
		"-concurrency", "1",
	}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	var output jsonReport
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if output.StatusCodes[http.StatusServiceUnavailable] != 1 {
		t.Fatalf("status codes = %#v", output.StatusCodes)
	}
}

func TestCancellationStopsSchedulingRequests(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, []string{
			"-target", server.URL,
			"-requests", "20",
			"-concurrency", "1",
			"-timeout", "5s",
		}, &stdout, &stderr)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()

	select {
	case exitCode := <-done:
		if exitCode != 1 {
			t.Fatalf("exit code = %d, want 1", exitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("load test did not stop after cancellation")
	}
	releaseOnce.Do(func() { close(release) })

	var output jsonReport
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode report: %v; output = %q", err, stdout.String())
	}
	if output.Total >= 20 {
		t.Fatalf("total = %d, want fewer than 20 after cancellation", output.Total)
	}
	if !strings.Contains(stderr.String(), "load test canceled") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestParseConfigReadsDiagramAndValidatesFlags(t *testing.T) {
	diagramPath := filepath.Join(t.TempDir(), "diagram.mmd")
	const diagram = "sequenceDiagram\nA->>B: hello\n"
	if err := os.WriteFile(diagramPath, []byte(diagram), 0o600); err != nil {
		t.Fatalf("write diagram: %v", err)
	}

	cfg, err := parseConfig([]string{
		"-target", "https://example.com",
		"-requests", "7",
		"-concurrency", "3",
		"-timeout", "2s",
		"-format", "SVG",
		"-diagram-file", diagramPath,
		"-unique",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.target != "https://example.com/render?format=svg" {
		t.Fatalf("target = %q", cfg.target)
	}
	if string(cfg.diagram) != diagram || cfg.total != 7 || cfg.concurrency != 3 || cfg.timeout != 2*time.Second || !cfg.unique {
		t.Fatalf("config = %#v", cfg)
	}

	emptyPath := filepath.Join(t.TempDir(), "empty.mmd")
	if err := os.WriteFile(emptyPath, []byte(" \n"), 0o600); err != nil {
		t.Fatalf("write empty diagram: %v", err)
	}
	tests := []struct {
		name string
		args []string
	}{
		{name: "zero requests", args: []string{"-requests", "0"}},
		{name: "too many requests", args: []string{"-requests", "1000001"}},
		{name: "zero concurrency", args: []string{"-concurrency", "0"}},
		{name: "zero timeout", args: []string{"-timeout", "0"}},
		{name: "bad format", args: []string{"-format", "pdf"}},
		{name: "bad scheme", args: []string{"-target", "ftp://example.com/render"}},
		{name: "missing host", args: []string{"-target", "/render"}},
		{name: "wrong path", args: []string{"-target", "https://example.com/wrong"}},
		{name: "fragment", args: []string{"-target", "https://example.com/render#fragment"}},
		{name: "empty diagram", args: []string{"-diagram-file", emptyPath}},
		{name: "positional argument", args: []string{"extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseConfig(tt.args, io.Discard); err == nil {
				t.Fatal("parseConfig() error = nil")
			}
		})
	}
}

func validTestConfig(t *testing.T, target string, total, concurrency int, unique bool, format string) config {
	t.Helper()
	cfg := config{
		target:      target,
		total:       total,
		concurrency: concurrency,
		timeout:     2 * time.Second,
		format:      format,
		diagram:     []byte(defaultDiagram),
		unique:      unique,
	}
	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("validateConfig() error = %v", err)
	}
	return cfg
}
