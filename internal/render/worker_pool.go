package render

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	workerProtocolVersion = 1
	maxProtocolLineBytes  = 64 * 1_024
	maxWorkerStderrBytes  = 8 * 1_024
	workerStopGracePeriod = 2 * time.Second
	workerRetryMinDelay   = 25 * time.Millisecond
	workerRetryMaxDelay   = time.Second
)

// WorkerPoolConfig configures persistent Node and Chromium renderer workers.
type WorkerPoolConfig struct {
	NodePath              string
	ScriptPath            string
	ChromiumPath          string
	TempDir               string
	MaxDiagramBytes       int64
	MaxOutputBytes        int64
	Workers               int
	QueueSize             int
	Timeout               time.Duration
	MaxRendersPerWorker   int
	MaxWorkerAge          time.Duration
	MaxWorkerRSSBytes     int64
	IsolateNetwork        bool
	IsolationLauncherPath string
	ExtraEnv              []string
}

// WorkerPool renders Mermaid diagrams with bounded persistent Node workers.
type WorkerPool struct {
	config       WorkerPoolConfig
	nodePath     string
	launcherPath string
	ctx          context.Context
	cancel       context.CancelFunc
	jobs         chan *workerPoolJob
	slots        chan struct{}

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
	workers   sync.WaitGroup
	ready     atomic.Int32
	nextID    atomic.Uint64
}

type workerPoolJob struct {
	ctx      context.Context
	request  Request
	response chan workerPoolResponse

	mu        sync.Mutex
	abandoned bool
}

type workerPoolResponse struct {
	result Result
	err    error
}

type workerRequest struct {
	Type       string `json:"type"`
	ID         uint64 `json:"id"`
	Diagram    string `json:"diagram"`
	Format     Format `json:"format"`
	OutputPath string `json:"outputPath"`
}

type workerResponse struct {
	Type  string `json:"type"`
	ID    uint64 `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Fatal bool   `json:"fatal,omitempty"`
}

type workerReady struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
}

type nodeWorker struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        *bufio.Reader
	stderr        *boundedWorkerLog
	started       time.Time
	renders       int
	done          chan struct{}
	processCancel context.CancelFunc

	exitMu  sync.Mutex
	exitErr error
}

// NewWorkerPool validates config, launches workers, and waits for readiness.
func NewWorkerPool(ctx context.Context, config WorkerPoolConfig) (*WorkerPool, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if config.NodePath == "" {
		config.NodePath = "node"
	}
	nodePath, err := exec.LookPath(config.NodePath)
	if err != nil {
		return nil, fmt.Errorf("find Node executable %q: %w", config.NodePath, err)
	}
	launcherPath := ""
	if config.IsolateNetwork {
		if config.IsolationLauncherPath == "" {
			return nil, errors.New("IsolationLauncherPath is required when network isolation is enabled")
		}
		launcherPath, err = exec.LookPath(config.IsolationLauncherPath)
		if err != nil {
			return nil, fmt.Errorf("find isolation launcher %q: %w", config.IsolationLauncherPath, err)
		}
	}
	if config.ScriptPath == "" {
		return nil, errors.New("ScriptPath is required")
	}
	if config.MaxDiagramBytes <= 0 {
		return nil, errors.New("MaxDiagramBytes must be positive")
	}
	if config.MaxOutputBytes <= 0 {
		return nil, errors.New("MaxOutputBytes must be positive")
	}
	if config.Workers <= 0 {
		return nil, errors.New("workers must be positive")
	}
	if config.QueueSize < 0 {
		return nil, errors.New("QueueSize must not be negative")
	}
	if config.QueueSize > int(^uint(0)>>1)-config.Workers {
		return nil, errors.New("workers plus queue size is too large")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	if config.MaxRendersPerWorker <= 0 {
		return nil, errors.New("MaxRendersPerWorker must be positive")
	}
	if config.MaxWorkerAge <= 0 {
		return nil, errors.New("MaxWorkerAge must be positive")
	}
	if config.MaxWorkerRSSBytes <= 0 {
		return nil, errors.New("MaxWorkerRSSBytes must be positive")
	}
	if err := validateWorkerEnvironment(config.ExtraEnv); err != nil {
		return nil, err
	}
	if config.TempDir != "" {
		info, err := os.Stat(config.TempDir)
		if err != nil {
			return nil, fmt.Errorf("validate TempDir: %w", err)
		}
		if !info.IsDir() {
			return nil, errors.New("TempDir must point to a directory")
		}
	}

	poolCtx, cancel := context.WithCancel(ctx)
	pool := &WorkerPool{
		config:       config,
		nodePath:     nodePath,
		launcherPath: launcherPath,
		ctx:          poolCtx,
		cancel:       cancel,
		jobs:         make(chan *workerPoolJob, config.Workers+config.QueueSize),
		slots:        make(chan struct{}, config.Workers+config.QueueSize),
	}

	started := make([]*nodeWorker, 0, config.Workers)
	for range config.Workers {
		worker, startErr := pool.startWorker(poolCtx)
		if startErr != nil {
			cancel()
			for _, existing := range started {
				existing.stop(false)
			}
			return nil, fmt.Errorf("start renderer worker: %w", startErr)
		}
		started = append(started, worker)
	}
	for _, worker := range started {
		pool.ready.Add(1)
		pool.workers.Add(1)
		go pool.runWorker(worker)
	}
	go func() {
		<-poolCtx.Done()
		_ = pool.Close()
	}()
	return pool, nil
}

// Ready reports whether the pool has at least one live, ready browser worker.
func (p *WorkerPool) Ready() bool {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	return !closed && p.ready.Load() > 0
}

// Render queues one render without exceeding the configured queue bound.
func (p *WorkerPool) Render(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, &Error{Code: CodeCanceled, Message: "rendering was canceled", Cause: context.Canceled}
	}
	if err := ValidateRequest(request, p.config.MaxDiagramBytes); err != nil {
		return Result{}, err
	}
	renderCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	if err := renderCtx.Err(); err != nil {
		return Result{}, contextError(err)
	}

	job := &workerPoolJob{
		ctx:      renderCtx,
		request:  request,
		response: make(chan workerPoolResponse, 1),
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Result{}, poolClosedError()
	}
	if p.ready.Load() == 0 {
		p.mu.Unlock()
		return Result{}, &Error{Code: CodeInternal, Message: "renderer worker is unavailable"}
	}
	select {
	case p.slots <- struct{}{}:
	default:
		p.mu.Unlock()
		return Result{}, &Error{Code: CodeOverloaded, Message: "renderer queue is full"}
	}
	select {
	case p.jobs <- job:
		p.mu.Unlock()
	case <-renderCtx.Done():
		<-p.slots
		p.mu.Unlock()
		return Result{}, contextError(renderCtx.Err())
	}

	select {
	case response := <-job.response:
		return response.result, response.err
	case <-renderCtx.Done():
		if response, completed := job.abandon(); completed {
			return response.result, response.err
		}
		return Result{}, contextError(renderCtx.Err())
	case <-p.ctx.Done():
		if response, completed := job.abandon(); completed {
			return response.result, response.err
		}
		return Result{}, poolClosedError()
	}
}

// Close stops workers and rejects queued and future renders. It is idempotent.
func (p *WorkerPool) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		p.mu.Unlock()

		p.workers.Wait()
		for {
			select {
			case job := <-p.jobs:
				job.complete(workerPoolResponse{err: poolClosedError()})
				<-p.slots
			default:
				return
			}
		}
	})
	return p.closeErr
}

func (p *WorkerPool) runWorker(worker *nodeWorker) {
	defer p.workers.Done()
	current := worker
	retryDelay := time.Duration(0)
	defer func() {
		if current != nil {
			p.retireWorker(current, false)
		}
	}()

	for {
		if current == nil {
			if retryDelay > 0 && !p.waitForWorkerRetry(retryDelay) {
				return
			}
			started, err := p.startWorker(p.ctx)
			if err != nil {
				if p.ctx.Err() != nil {
					return
				}
				retryDelay = nextWorkerRetryDelay(retryDelay)
				continue
			}
			current = started
			p.ready.Add(1)
			retryDelay = 0
			continue
		}

		select {
		case <-p.ctx.Done():
			return
		case <-current.done:
			p.retireWorker(current, false)
			current = nil
			retryDelay = workerRetryMinDelay
		case job := <-p.jobs:
			if p.shouldRecycle(current) {
				p.retireWorker(current, true)
				current = nil
				if p.ctx.Err() != nil {
					p.failJob(job, poolClosedError())
					continue
				}
				started, err := p.startWorker(job.ctx)
				if err != nil {
					p.failJob(job, p.workerStartError(job.ctx, err))
					retryDelay = workerRetryMinDelay
					continue
				}
				current = started
				p.ready.Add(1)
			}
			if p.runJob(current, job) {
				current = nil
				retryDelay = workerRetryMinDelay
			}
		}
	}
}

func (p *WorkerPool) waitForWorkerRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return false
		case <-timer.C:
			return true
		case job := <-p.jobs:
			p.failJob(job, &Error{Code: CodeInternal, Message: "renderer worker is unavailable"})
		}
	}
}

func nextWorkerRetryDelay(previous time.Duration) time.Duration {
	if previous < workerRetryMinDelay {
		return workerRetryMinDelay
	}
	if previous >= workerRetryMaxDelay/2 {
		return workerRetryMaxDelay
	}
	return previous * 2
}

func (p *WorkerPool) runJob(worker *nodeWorker, job *workerPoolJob) bool {
	result, badWorker, err := p.renderWithWorker(job.ctx, worker, job.request)
	if badWorker {
		p.retireWorker(worker, false)
	}
	job.complete(workerPoolResponse{result: result, err: err})
	<-p.slots
	return badWorker
}

func (p *WorkerPool) failJob(job *workerPoolJob, err error) {
	job.complete(workerPoolResponse{err: err})
	<-p.slots
}

func (j *workerPoolJob) complete(response workerPoolResponse) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.abandoned {
		if response.result.ReadCloser != nil {
			_ = response.result.Close()
		}
		return
	}
	j.response <- response
}

func (j *workerPoolJob) abandon() (workerPoolResponse, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	select {
	case response := <-j.response:
		return response, true
	default:
		j.abandoned = true
		return workerPoolResponse{}, false
	}
}

func (p *WorkerPool) renderWithWorker(
	ctx context.Context,
	worker *nodeWorker,
	request Request,
) (result Result, badWorker bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Result{}, false, contextError(err)
	}
	dir, err := os.MkdirTemp(p.config.TempDir, "mermaid-worker-render-*")
	if err != nil {
		return Result{}, false, &Error{Code: CodeInternal, Message: "could not prepare rendering", Cause: err}
	}
	defer func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			cleanupErr := &Error{Code: CodeInternal, Message: "could not remove temporary files", Cause: removeErr}
			result = Result{}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()

	id := p.nextID.Add(1)
	outputPath := filepath.Join(dir, "diagram."+string(request.Format))
	response, err := p.exchange(ctx, worker, workerRequest{
		Type:       "render",
		ID:         id,
		Diagram:    request.Diagram,
		Format:     request.Format,
		OutputPath: outputPath,
	})
	worker.renders++
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, true, contextError(ctx.Err())
		}
		if p.ctx.Err() != nil {
			return Result{}, true, poolClosedError()
		}
		return Result{}, true, &Error{
			Code:    CodeInternal,
			Message: "renderer worker failed",
			Cause:   fmt.Errorf("%w; stderr: %s", err, worker.stderr.String()),
		}
	}
	if response.Type != "result" || response.ID != id {
		return Result{}, true, &Error{
			Code:    CodeInternal,
			Message: "renderer worker failed",
			Cause:   fmt.Errorf("invalid renderer response type %q or id %d", response.Type, response.ID),
		}
	}
	if !response.OK {
		if response.Kind == "too_large" {
			return Result{}, false, &Error{Code: CodeTooLarge, Message: "rendered image exceeds a safety limit"}
		}
		if response.Kind != "render" {
			return Result{}, true, &Error{
				Code:    CodeInternal,
				Message: "renderer worker failed",
				Cause:   errors.New("renderer reported an internal failure"),
			}
		}
		detail := sanitizeRendererMessage(response.Error, dir)
		message := "Mermaid could not render the diagram"
		if detail != "" {
			message += ": " + detail
		}
		return Result{}, response.Fatal, &Error{
			Code:    CodeRenderRejected,
			Message: message,
			Cause:   errors.New("renderer rejected diagram"),
		}
	}
	if response.Fatal {
		return Result{}, true, &Error{Code: CodeInternal, Message: "renderer worker failed"}
	}

	file, info, err := openWorkerOutput(outputPath)
	if err != nil {
		return Result{}, true, &Error{Code: CodeInternal, Message: "renderer produced no image", Cause: err}
	}
	defer func() { _ = file.Close() }()
	if info.Size() > p.config.MaxOutputBytes {
		return Result{}, false, &Error{
			Code:    CodeTooLarge,
			Message: fmt.Sprintf("rendered image exceeds the %d-byte limit", p.config.MaxOutputBytes),
		}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return Result{}, true, &Error{Code: CodeInternal, Message: "could not read rendered image", Cause: err}
	}
	data, err = sanitizeOutput(request.Format, data)
	if err != nil {
		return Result{}, true, err
	}
	if err := validateOutput(request.Format, data, p.config.MaxOutputBytes); err != nil {
		var renderErr *Error
		if !errors.As(err, &renderErr) {
			return Result{}, true, err
		}
		bad := renderErr.Code != CodeTooLarge && renderErr.Code != CodeRenderRejected
		return Result{}, bad, err
	}
	return Result{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		MIMEType:   request.Format.MIMEType(),
		Size:       int64(len(data)),
	}, false, nil
}

func (p *WorkerPool) exchange(
	ctx context.Context,
	worker *nodeWorker,
	request workerRequest,
) (workerResponse, error) {
	type exchangeResult struct {
		response workerResponse
		err      error
	}
	finished := make(chan exchangeResult, 1)
	go func() {
		if err := writeJSONLine(worker.stdin, request); err != nil {
			finished <- exchangeResult{err: fmt.Errorf("write renderer request: %w", err)}
			return
		}
		var response workerResponse
		if err := readJSONLine(worker.stdout, &response); err != nil {
			finished <- exchangeResult{err: fmt.Errorf("read renderer response: %w", err)}
			return
		}
		finished <- exchangeResult{response: response}
	}()

	select {
	case result := <-finished:
		return result.response, result.err
	case <-ctx.Done():
		worker.stop(false)
		return workerResponse{}, ctx.Err()
	case <-p.ctx.Done():
		worker.stop(false)
		return workerResponse{}, p.ctx.Err()
	case <-worker.done:
		return workerResponse{}, fmt.Errorf("renderer exited: %w", worker.exitError())
	}
}

func (p *WorkerPool) startWorker(ctx context.Context) (*nodeWorker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	startCtx, cancelStart := context.WithTimeout(ctx, p.config.Timeout)
	defer cancelStart()
	processCtx, processCancel := context.WithCancel(context.Background())
	command := p.nodePath
	arguments := []string{p.config.ScriptPath}
	if p.config.IsolateNetwork {
		command = p.launcherPath
		arguments = []string{p.nodePath, p.config.ScriptPath}
	}
	cmd := exec.CommandContext(processCtx, command, arguments...)
	configureCommand(cmd)
	cmd.Env = p.workerEnvironment()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		processCancel()
		return nil, fmt.Errorf("open worker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		processCancel()
		return nil, fmt.Errorf("open worker stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		processCancel()
		return nil, fmt.Errorf("open worker stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		processCancel()
		return nil, fmt.Errorf("start worker process: %w", err)
	}
	worker := &nodeWorker{
		cmd:           cmd,
		stdin:         stdin,
		stdout:        bufio.NewReaderSize(stdout, maxProtocolLineBytes),
		stderr:        &boundedWorkerLog{limit: maxWorkerStderrBytes},
		started:       time.Now(),
		done:          make(chan struct{}),
		processCancel: processCancel,
	}
	go func() {
		_, _ = io.Copy(worker.stderr, stderr)
	}()
	go func() {
		err := cmd.Wait()
		worker.exitMu.Lock()
		worker.exitErr = err
		worker.exitMu.Unlock()
		close(worker.done)
	}()

	readyCh := make(chan error, 1)
	go func() {
		var ready workerReady
		if err := readJSONLine(worker.stdout, &ready); err != nil {
			readyCh <- fmt.Errorf("read readiness: %w", err)
			return
		}
		if ready.Type != "ready" || ready.Protocol != workerProtocolVersion {
			readyCh <- fmt.Errorf("invalid readiness response type %q protocol %d", ready.Type, ready.Protocol)
			return
		}
		readyCh <- nil
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			worker.stop(false)
			return nil, err
		}
		return worker, nil
	case <-startCtx.Done():
		worker.stop(false)
		return nil, startCtx.Err()
	case <-p.ctx.Done():
		worker.stop(false)
		return nil, p.ctx.Err()
	case <-worker.done:
		worker.stop(false)
		return nil, fmt.Errorf("worker exited before readiness: %w; stderr: %s", worker.exitError(), worker.stderr.String())
	}
}

func (p *WorkerPool) shouldRecycle(worker *nodeWorker) bool {
	if worker.renders >= p.config.MaxRendersPerWorker || time.Since(worker.started) >= p.config.MaxWorkerAge {
		return true
	}
	rss, supported := processGroupRSS(worker.cmd.Process.Pid)
	return supported && rss > p.config.MaxWorkerRSSBytes
}

func (p *WorkerPool) retireWorker(worker *nodeWorker, graceful bool) {
	p.ready.Add(-1)
	worker.stop(graceful)
}

func (p *WorkerPool) workerStartError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return contextError(ctx.Err())
	}
	if p.ctx.Err() != nil {
		return poolClosedError()
	}
	return &Error{Code: CodeInternal, Message: "renderer worker failed", Cause: err}
}

func (w *nodeWorker) stop(graceful bool) {
	if graceful {
		_ = w.stdin.Close()
	} else {
		w.killProcessGroup()
	}
	select {
	case <-w.done:
		w.processCancel()
		return
	case <-time.After(workerStopGracePeriod):
		w.killProcessGroup()
	}
	select {
	case <-w.done:
	case <-time.After(workerStopGracePeriod):
	}
}

func (w *nodeWorker) killProcessGroup() {
	if w.cmd.Cancel != nil {
		_ = w.cmd.Cancel()
	}
	w.processCancel()
}

func (w *nodeWorker) exitError() error {
	w.exitMu.Lock()
	defer w.exitMu.Unlock()
	if w.exitErr == nil {
		return errors.New("renderer process exited")
	}
	return w.exitErr
}

func openWorkerOutput(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("worker output is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, nil, errors.New("worker output changed before it could be read")
	}
	return file, openedInfo, nil
}

func writeJSONLine(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func readJSONLine(reader *bufio.Reader, value any) error {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return errors.New("renderer protocol line exceeds limit")
	}
	if err != nil {
		return err
	}
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return errors.New("empty renderer protocol line")
	}
	if err := json.Unmarshal(line, value); err != nil {
		return err
	}
	return nil
}

func (p *WorkerPool) workerEnvironment() []string {
	keys := []string{
		"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ",
		"SystemRoot", "ComSpec", "PATHEXT", "USERPROFILE",
	}
	env := make([]string, 0, len(keys)+len(p.config.ExtraEnv)+2)
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, p.config.ExtraEnv...)
	env = append(env, fmt.Sprintf("MERMAID_MAX_OUTPUT_BYTES=%d", p.config.MaxOutputBytes))
	if p.config.ChromiumPath != "" {
		env = append(env, "MERMAID_CHROMIUM_PATH="+p.config.ChromiumPath)
	}
	return env
}

func validateWorkerEnvironment(env []string) error {
	for _, value := range env {
		if strings.IndexByte(value, 0) >= 0 {
			return errors.New("ExtraEnv contains a NUL byte")
		}
		if index := strings.IndexByte(value, '='); index <= 0 {
			return fmt.Errorf("invalid ExtraEnv entry %q", value)
		}
	}
	return nil
}

func poolClosedError() error {
	return &Error{Code: CodeInternal, Message: "renderer is closed"}
}

type boundedWorkerLog struct {
	mu     sync.Mutex
	buffer []byte
	limit  int
}

func (w *boundedWorkerLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - len(w.buffer)
	if remaining > 0 {
		w.buffer = append(w.buffer, data[:min(len(data), remaining)]...)
	}
	return len(data), nil
}

func (w *boundedWorkerLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buffer)
}
