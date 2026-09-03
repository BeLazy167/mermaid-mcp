package render

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkerScriptSecurityPolicy(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "renderer", "worker.mjs"))
	if err != nil {
		t.Fatalf("read worker script: %v", err)
	}
	text := string(source)
	for _, required := range []string{
		`securityLevel: "strict"`,
		"pipe: true",
		"maxTextSize: 50_000",
		"maxEdges: 500",
		"htmlLabels: false",
		"iconPacks: Object.freeze([])",
		"browser.createBrowserContext()",
		"await context.close()",
		"--disable-background-networking",
		"--proxy-server=http://0.0.0.0:9",
		"--host-resolver-rules=MAP * 0.0.0.0",
		"MERMAID_MAX_OUTPUT_BYTES",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("worker script missing %q", required)
		}
	}
	if strings.Contains(text, "--no-sandbox") {
		t.Fatal("worker script disables Chromium sandbox")
	}
}

func TestDockerfileKeepsChromiumInWorkerProcessGroup(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	patch, err := os.ReadFile(filepath.Join("..", "..", "renderer", "patch-dependencies.mjs"))
	if err != nil {
		t.Fatalf("read dependency patch: %v", err)
	}
	if !strings.Contains(string(dockerfile), "node patch-dependencies.mjs") ||
		!strings.Contains(string(patch), `opts.detached ??= false;`) ||
		!strings.Contains(string(patch), `unexpected dependency source`) {
		t.Fatal("image does not build-gate the Puppeteer process-group patch")
	}
}

func TestWorkerPoolReusesWorkerAndCleansOutput(t *testing.T) {
	pool, stateDir := newTestWorkerPool(t, testWorkerPoolConfig())

	if !pool.Ready() {
		t.Fatal("Ready() = false after startup")
	}
	for _, format := range []Format{FormatSVG, FormatPNG} {
		result, err := pool.Render(context.Background(), Request{
			Diagram: "graph TD; A-->B",
			Format:  format,
		})
		if err != nil {
			t.Fatalf("Render(%s) error = %v", format, err)
		}
		data, err := io.ReadAll(result)
		if err != nil {
			t.Fatalf("read result: %v", err)
		}
		if err := validateOutput(format, data, int64(len(data))); err != nil {
			t.Fatalf("invalid %s data: %v", format, err)
		}
		if matches, err := filepath.Glob(filepath.Join(stateDir, "mermaid-worker-render-*")); err != nil || len(matches) != 0 {
			t.Fatalf("temporary output directories = %v, error = %v", matches, err)
		}
		if err := result.Close(); err != nil {
			t.Fatalf("close result: %v", err)
		}
	}

	if starts := readWorkerStarts(t, stateDir); len(starts) != 1 {
		t.Fatalf("worker starts = %v, want one reused process", starts)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if pool.Ready() {
		t.Fatal("Ready() = true after Close")
	}
}

func TestWorkerPoolSupportsNoWaitingQueue(t *testing.T) {
	config := testWorkerPoolConfig()
	config.QueueSize = 0
	pool, _ := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)

	result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("close result: %v", err)
	}
}

func TestWorkerPoolRejectsWhenQueueIsFull(t *testing.T) {
	config := testWorkerPoolConfig()
	config.QueueSize = 1
	pool, stateDir := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)

	firstDone := make(chan error, 1)
	go func() {
		result, err := pool.Render(context.Background(), Request{Diagram: "BLOCK", Format: FormatSVG})
		if err == nil {
			err = result.Close()
		}
		firstDone <- err
	}()
	waitForFile(t, filepath.Join(stateDir, "blocked"))

	secondDone := make(chan error, 1)
	go func() {
		result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; B-->C", Format: FormatSVG})
		if err == nil {
			err = result.Close()
		}
		secondDone <- err
	}()
	waitForCondition(t, func() bool { return len(pool.jobs) == 1 }, "second render was not queued")

	_, err := pool.Render(context.Background(), Request{Diagram: "graph TD; C-->D", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeOverloaded)

	if err := os.WriteFile(filepath.Join(stateDir, "release"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	for name, done := range map[string]<-chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s render error = %v", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s render did not finish", name)
		}
	}
}

func TestWorkerStartupCrashKillsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group assertion requires Unix signals")
	}
	stateDir := t.TempDir()
	config := testWorkerPoolConfig()
	config.TempDir = t.TempDir()
	config.ExtraEnv = []string{
		"GO_WANT_WORKER_POOL_HELPER=1",
		"FAKE_STATE_DIR=" + stateDir,
		"FAKE_STARTUP=crash-child",
	}
	if pool, err := NewWorkerPool(context.Background(), config); err == nil {
		_ = pool.Close()
		t.Fatal("NewWorkerPool() error = nil")
	}
	childPIDData, err := os.ReadFile(filepath.Join(stateDir, "startup-child-pid"))
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	childPID := strings.TrimSpace(string(childPIDData))
	t.Cleanup(func() { _ = exec.Command("kill", "-9", childPID).Run() })
	waitForCondition(t, func() bool {
		return exec.Command("kill", "-0", childPID).Run() != nil
	}, "startup worker descendant remained alive")
}

func TestWorkerCrashKillsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group assertion requires Unix signals")
	}
	pool, stateDir := newTestWorkerPool(t, testWorkerPoolConfig())
	defer closeWorkerPool(t, pool)

	_, err := pool.Render(context.Background(), Request{Diagram: "CRASH_WITH_CHILD", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeInternal)
	childPIDData, err := os.ReadFile(filepath.Join(stateDir, "child-pid"))
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	childPID := strings.TrimSpace(string(childPIDData))
	t.Cleanup(func() { _ = exec.Command("kill", "-9", childPID).Run() })
	waitForCondition(t, func() bool {
		return exec.Command("kill", "-0", childPID).Run() != nil
	}, "crashed worker descendant remained alive")
	waitForCondition(t, func() bool { return pool.Ready() }, "worker did not recover")
}

func TestWorkerPoolTimeoutKillsAndRestartsWorker(t *testing.T) {
	config := testWorkerPoolConfig()
	config.Timeout = 50 * time.Millisecond
	pool, stateDir := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)

	_, err := pool.Render(context.Background(), Request{Diagram: "SLOW", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeTimeout)
	waitForCondition(t, func() bool {
		return len(readWorkerStartsIfPresent(stateDir)) >= 2 && pool.Ready()
	}, "worker did not recover after timeout")

	result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
	if err != nil {
		t.Fatalf("render after timeout: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("close result: %v", err)
	}
	if starts := readWorkerStarts(t, stateDir); len(starts) < 2 {
		t.Fatalf("worker starts = %v, want at least one restart after timeout", starts)
	}
}

func TestWorkerPoolCrashAndProtocolFailureRestartWorker(t *testing.T) {
	tests := []struct {
		name    string
		diagram string
	}{
		{name: "crash", diagram: "CRASH"},
		{name: "malformed response", diagram: "MALFORMED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, stateDir := newTestWorkerPool(t, testWorkerPoolConfig())
			defer closeWorkerPool(t, pool)

			_, err := pool.Render(context.Background(), Request{Diagram: tt.diagram, Format: FormatSVG})
			assertRenderErrorCode(t, err, CodeInternal)
			waitForCondition(t, pool.Ready, "worker did not recover after failure")

			result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
			if err != nil {
				t.Fatalf("render after worker failure: %v", err)
			}
			if err := result.Close(); err != nil {
				t.Fatalf("close result: %v", err)
			}
			if starts := readWorkerStarts(t, stateDir); len(starts) != 2 {
				t.Fatalf("worker starts = %v, want restart", starts)
			}
		})
	}
}

func TestWorkerPoolRecoversReadinessWithoutTraffic(t *testing.T) {
	config := testWorkerPoolConfig()
	config.ExtraEnv = append(config.ExtraEnv, "FAKE_EXIT_ONCE_AFTER_READY=1")
	pool, stateDir := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)

	waitForCondition(t, func() bool {
		return len(readWorkerStartsIfPresent(stateDir)) >= 2 && pool.Ready()
	}, "worker did not restart eagerly")
}

func TestWorkerPoolQueuedCancellationReturnsImmediately(t *testing.T) {
	config := testWorkerPoolConfig()
	config.QueueSize = 1
	pool, stateDir := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)

	activeDone := make(chan error, 1)
	go func() {
		result, err := pool.Render(context.Background(), Request{Diagram: "BLOCK", Format: FormatSVG})
		if err == nil {
			err = result.Close()
		}
		activeDone <- err
	}()
	waitForFile(t, filepath.Join(stateDir, "blocked"))

	ctx, cancel := context.WithCancel(context.Background())
	queuedDone := make(chan error, 1)
	go func() {
		_, err := pool.Render(ctx, Request{Diagram: "graph TD; B-->C", Format: FormatSVG})
		queuedDone <- err
	}()
	waitForCondition(t, func() bool { return len(pool.jobs) == 1 }, "render was not queued")
	cancel()
	select {
	case err := <-queuedDone:
		assertRenderErrorCode(t, err, CodeCanceled)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("queued render did not return on cancellation")
	}

	if err := os.WriteFile(filepath.Join(stateDir, "release"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	select {
	case err := <-activeDone:
		if err != nil {
			t.Fatalf("active render error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active render did not finish")
	}
}

func TestWorkerPoolDoesNotInheritGatewaySecrets(t *testing.T) {
	t.Setenv("R2_SECRET_ACCESS_KEY", "must-not-reach-worker")
	t.Setenv("CLUSTER_ADMISSION_TOKEN", "also-must-not-reach-worker")
	config := testWorkerPoolConfig()
	config.ExtraEnv = append(config.ExtraEnv, "FAKE_ASSERT_NO_GATEWAY_SECRET=1")
	pool, _ := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)
	if !pool.Ready() {
		t.Fatal("worker did not become ready")
	}
}

func TestWorkerPoolValidatesOutput(t *testing.T) {
	tests := []struct {
		name      string
		diagram   string
		maxOutput int64
		wantCode  ErrorCode
	}{
		{name: "missing", diagram: "MISSING", maxOutput: 1_024, wantCode: CodeInternal},
		{name: "invalid magic", diagram: "INVALID", maxOutput: 1_024, wantCode: CodeInternal},
		{name: "oversized", diagram: "OVERSIZE", maxOutput: 16, wantCode: CodeTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testWorkerPoolConfig()
			config.MaxOutputBytes = tt.maxOutput
			pool, _ := newTestWorkerPool(t, config)
			defer closeWorkerPool(t, pool)

			_, err := pool.Render(context.Background(), Request{Diagram: tt.diagram, Format: FormatPNG})
			assertRenderErrorCode(t, err, tt.wantCode)
			assertNoRenderDirectories(t, pool.config.TempDir)
		})
	}
}

func TestWorkerPoolRejectsUnsafeSVGWithoutRestart(t *testing.T) {
	pool, stateDir := newTestWorkerPool(t, testWorkerPoolConfig())
	defer closeWorkerPool(t, pool)

	_, err := pool.Render(context.Background(), Request{Diagram: "UNSAFE", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeRenderRejected)

	result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
	if err != nil {
		t.Fatalf("render after unsafe SVG: %v", err)
	}
	_ = result.Close()
	if starts := readWorkerStarts(t, stateDir); len(starts) != 1 {
		t.Fatalf("worker starts = %v, want validation rejection to preserve worker", starts)
	}
}

func TestWorkerPoolReturnsRendererRejectionWithoutRestart(t *testing.T) {
	pool, stateDir := newTestWorkerPool(t, testWorkerPoolConfig())
	defer closeWorkerPool(t, pool)

	_, err := pool.Render(context.Background(), Request{Diagram: "REJECT", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeRenderRejected)
	if strings.Contains(err.Error(), testWorkerStateDir(t, pool)) {
		t.Fatalf("renderer error leaks temporary path: %v", err)
	}

	result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
	if err != nil {
		t.Fatalf("render after rejection: %v", err)
	}
	_ = result.Close()
	if starts := readWorkerStarts(t, stateDir); len(starts) != 1 {
		t.Fatalf("worker starts = %v, want rejection to preserve worker", starts)
	}
}

func TestWorkerPoolRecyclesByCountAndAge(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		config := testWorkerPoolConfig()
		config.MaxRendersPerWorker = 1
		pool, stateDir := newTestWorkerPool(t, config)
		defer closeWorkerPool(t, pool)

		for range 2 {
			result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			_ = result.Close()
		}
		if starts := readWorkerStarts(t, stateDir); len(starts) != 2 {
			t.Fatalf("worker starts = %v, want count recycle", starts)
		}
	})

	t.Run("age", func(t *testing.T) {
		config := testWorkerPoolConfig()
		config.MaxWorkerAge = 30 * time.Millisecond
		pool, stateDir := newTestWorkerPool(t, config)
		defer closeWorkerPool(t, pool)

		time.Sleep(50 * time.Millisecond)
		result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		_ = result.Close()
		if starts := readWorkerStarts(t, stateDir); len(starts) != 2 {
			t.Fatalf("worker starts = %v, want age recycle", starts)
		}
	})
}

func TestWorkerPoolCloseStopsActiveAndQueuedRenders(t *testing.T) {
	config := testWorkerPoolConfig()
	config.QueueSize = 1
	pool, stateDir := newTestWorkerPool(t, config)

	activeDone := make(chan error, 1)
	go func() {
		_, err := pool.Render(context.Background(), Request{Diagram: "BLOCK", Format: FormatSVG})
		activeDone <- err
	}()
	waitForFile(t, filepath.Join(stateDir, "blocked"))

	queuedDone := make(chan error, 1)
	go func() {
		_, err := pool.Render(context.Background(), Request{Diagram: "graph TD; B-->C", Format: FormatSVG})
		queuedDone <- err
	}()
	waitForCondition(t, func() bool { return len(pool.jobs) == 1 }, "render was not queued")

	closed := make(chan error, 1)
	go func() { closed <- pool.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not finish")
	}

	for name, done := range map[string]<-chan error{"active": activeDone, "queued": queuedDone} {
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s render error = nil after Close", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s render did not stop", name)
		}
	}
	if pool.Ready() {
		t.Fatal("Ready() = true after Close")
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	_, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeInternal)
}

func TestNewWorkerPoolRejectsInvalidConfigAndReadiness(t *testing.T) {
	base := testWorkerPoolConfig()
	tests := []struct {
		name   string
		change func(*WorkerPoolConfig)
	}{
		{name: "missing node", change: func(c *WorkerPoolConfig) { c.NodePath = filepath.Join(t.TempDir(), "missing") }},
		{name: "missing script", change: func(c *WorkerPoolConfig) { c.ScriptPath = "" }},
		{name: "diagram limit", change: func(c *WorkerPoolConfig) { c.MaxDiagramBytes = 0 }},
		{name: "output limit", change: func(c *WorkerPoolConfig) { c.MaxOutputBytes = 0 }},
		{name: "workers", change: func(c *WorkerPoolConfig) { c.Workers = 0 }},
		{name: "queue", change: func(c *WorkerPoolConfig) { c.QueueSize = -1 }},
		{name: "timeout", change: func(c *WorkerPoolConfig) { c.Timeout = 0 }},
		{name: "render recycle", change: func(c *WorkerPoolConfig) { c.MaxRendersPerWorker = 0 }},
		{name: "age recycle", change: func(c *WorkerPoolConfig) { c.MaxWorkerAge = 0 }},
		{name: "RSS recycle", change: func(c *WorkerPoolConfig) { c.MaxWorkerRSSBytes = 0 }},
		{name: "network isolation launcher", change: func(c *WorkerPoolConfig) { c.IsolateNetwork = true; c.IsolationLauncherPath = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := base
			tt.change(&config)
			pool, err := NewWorkerPool(context.Background(), config)
			if err == nil {
				_ = pool.Close()
				t.Fatal("NewWorkerPool() error = nil")
			}
		})
	}

	config := base
	config.ExtraEnv = append(config.ExtraEnv, "FAKE_STARTUP=bad-ready")
	if pool, err := NewWorkerPool(context.Background(), config); err == nil {
		_ = pool.Close()
		t.Fatal("NewWorkerPool() accepted invalid readiness response")
	}
}

// TestWorkerPoolProtocolPeer runs only in subprocesses started by WorkerPool.
func TestWorkerPoolProtocolPeer(t *testing.T) {
	if os.Getenv("GO_WANT_WORKER_POOL_HELPER") != "1" {
		return
	}
	fakeWorkerProtocolPeer()
	os.Exit(0)
}

type fakeWorkerRequest struct {
	Type       string `json:"type"`
	ID         uint64 `json:"id"`
	Diagram    string `json:"diagram"`
	Format     Format `json:"format"`
	OutputPath string `json:"outputPath"`
}

func fakeWorkerProtocolPeer() {
	stateDir := os.Getenv("FAKE_STATE_DIR")
	startFile := filepath.Join(stateDir, "starts")
	file, err := os.OpenFile(startFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(file, os.Getpid())
	_ = file.Close()

	if os.Getenv("FAKE_STARTUP") == "bad-ready" {
		fmt.Println(`{"type":"wrong"}`)
		return
	}
	if os.Getenv("FAKE_STARTUP") == "crash-child" {
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			os.Exit(24)
		}
		_ = os.WriteFile(filepath.Join(stateDir, "startup-child-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	if os.Getenv("FAKE_ASSERT_NO_GATEWAY_SECRET") == "1" && (os.Getenv("R2_SECRET_ACCESS_KEY") != "" || os.Getenv("CLUSTER_ADMISSION_TOKEN") != "") {
		fmt.Println(`{"type":"secret-leaked"}`)
		return
	}
	if err := encoder.Encode(map[string]any{"type": "ready", "protocol": 1}); err != nil {
		os.Exit(2)
	}
	if os.Getenv("FAKE_EXIT_ONCE_AFTER_READY") == "1" {
		marker := filepath.Join(stateDir, "exited-once")
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = file.Close()
			return
		}
	}

	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	for {
		var request fakeWorkerRequest
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			os.Exit(3)
		}
		switch request.Diagram {
		case "CRASH_WITH_CHILD":
			child := exec.Command("sleep", "60")
			if err := child.Start(); err != nil {
				os.Exit(24)
			}
			_ = os.WriteFile(filepath.Join(stateDir, "child-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
			os.Exit(23)
		case "CRASH":
			os.Exit(23)
		case "MALFORMED":
			fmt.Println("{broken")
			continue
		case "SLOW":
			time.Sleep(2 * time.Second)
		case "BLOCK":
			_ = os.WriteFile(filepath.Join(stateDir, "blocked"), []byte("1"), 0o600)
			for {
				if _, err := os.Stat(filepath.Join(stateDir, "release")); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		case "REJECT":
			_ = encoder.Encode(map[string]any{
				"type": "result", "id": request.ID, "ok": false, "kind": "render",
				"error": "bad diagram in " + filepath.Dir(request.OutputPath),
			})
			continue
		case "MISSING":
			_ = encoder.Encode(map[string]any{"type": "result", "id": request.ID, "ok": true})
			continue
		}

		var data []byte
		switch request.Diagram {
		case "INVALID":
			data = []byte("not an image")
		case "UNSAFE":
			data = []byte("<svg><script>alert(1)</script></svg>")
		case "OVERSIZE":
			data = append(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...), pngIEND...)
		default:
			if request.Format == FormatPNG {
				data = testPNG(1, 1)
			} else {
				data = []byte("<svg>rendered</svg>")
			}
		}
		if err := os.WriteFile(request.OutputPath, data, 0o600); err != nil {
			_ = encoder.Encode(map[string]any{
				"type": "result", "id": request.ID, "ok": false, "error": err.Error(), "fatal": true,
			})
			continue
		}
		_ = encoder.Encode(map[string]any{"type": "result", "id": request.ID, "ok": true})
	}
}

func testWorkerPoolConfig() WorkerPoolConfig {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return WorkerPoolConfig{
		NodePath:            executable,
		ScriptPath:          "-test.run=^TestWorkerPoolProtocolPeer$",
		TempDir:             "",
		MaxDiagramBytes:     1_024,
		MaxOutputBytes:      1_024,
		Workers:             1,
		QueueSize:           1,
		Timeout:             5 * time.Second,
		MaxRendersPerWorker: 100,
		MaxWorkerAge:        time.Hour,
		MaxWorkerRSSBytes:   1 << 30,
	}
}

func newTestWorkerPool(t *testing.T, config WorkerPoolConfig) (*WorkerPool, string) {
	t.Helper()
	stateDir := t.TempDir()
	if config.TempDir == "" {
		config.TempDir = filepath.Join(stateDir, "renders")
		if err := os.Mkdir(config.TempDir, 0o700); err != nil {
			t.Fatalf("create render temp: %v", err)
		}
	}
	config.ExtraEnv = append(config.ExtraEnv,
		"GO_WANT_WORKER_POOL_HELPER=1",
		"FAKE_STATE_DIR="+stateDir,
	)
	pool, err := NewWorkerPool(context.Background(), config)
	if err != nil {
		t.Fatalf("NewWorkerPool() error = %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool, stateDir
}

func closeWorkerPool(t *testing.T, pool *WorkerPool) {
	t.Helper()
	if err := pool.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func readWorkerStarts(t *testing.T, stateDir string) []int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "starts"))
	if err != nil {
		t.Fatalf("read starts: %v", err)
	}
	var starts []int
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("parse PID %q: %v", line, err)
		}
		starts = append(starts, pid)
	}
	return starts
}

func readWorkerStartsIfPresent(stateDir string) []string {
	data, err := os.ReadFile(filepath.Join(stateDir, "starts"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	waitForCondition(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, "file did not appear: "+path)
}

func waitForCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func assertNoRenderDirectories(t *testing.T, tempDir string) {
	t.Helper()
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read render temp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary render files remain: %v", entries)
	}
}

func testWorkerStateDir(t *testing.T, pool *WorkerPool) string {
	t.Helper()
	for _, env := range pool.config.ExtraEnv {
		if strings.HasPrefix(env, "FAKE_STATE_DIR=") {
			return strings.TrimPrefix(env, "FAKE_STATE_DIR=")
		}
	}
	return ""
}
