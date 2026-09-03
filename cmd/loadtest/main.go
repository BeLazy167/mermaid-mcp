package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTarget      = "http://localhost:8080/render"
	defaultRequests    = 100
	maxRequests        = 1_000_000
	defaultConcurrency = 10
	defaultTimeout     = 30 * time.Second
	defaultDiagram     = "graph TD\nA-->B\n"
)

type config struct {
	target      string
	total       int
	concurrency int
	timeout     time.Duration
	format      string
	diagram     []byte
	unique      bool
}

type requestResult struct {
	latency    time.Duration
	bytes      int64
	statusCode int
	failed     bool
}

type report struct {
	total             int
	success           int
	errors            int
	elapsed           time.Duration
	requestsPerSecond float64
	bytes             int64
	p50               time.Duration
	p95               time.Duration
	p99               time.Duration
	maxLatency        time.Duration
	statusCodes       map[int]int
}

type jsonReport struct {
	Total             int         `json:"total"`
	Success           int         `json:"success"`
	Errors            int         `json:"errors"`
	Elapsed           string      `json:"elapsed"`
	RequestsPerSecond float64     `json:"requests_per_second"`
	Bytes             int64       `json:"bytes"`
	P50               string      `json:"p50"`
	P95               string      `json:"p95"`
	P99               string      `json:"p99"`
	MaxLatency        string      `json:"max_latency"`
	StatusCodes       map[int]int `json:"status_codes"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(cli(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func cli(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := run(ctx, args, stdout, stderr)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	_, _ = fmt.Fprintln(stderr, err)
	return 1
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		return err
	}

	client := newHTTPClient(min(cfg.concurrency, cfg.total))
	defer client.CloseIdleConnections()

	result := execute(ctx, client, cfg)
	if err := json.NewEncoder(stdout).Encode(toJSONReport(result)); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if ctx.Err() != nil && result.total < cfg.total {
		return fmt.Errorf("load test canceled after %d of %d requests: %w", result.total, cfg.total, ctx.Err())
	}
	if result.errors > 0 {
		return fmt.Errorf("%d of %d requests failed", result.errors, result.total)
	}
	return nil
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	cfg := config{}
	var diagramFile string

	flags := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.target, "target", defaultTarget, "URL of the /render endpoint")
	flags.IntVar(&cfg.total, "requests", defaultRequests, "total requests (maximum 1000000)")
	flags.IntVar(&cfg.concurrency, "concurrency", defaultConcurrency, "maximum concurrent requests")
	flags.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "timeout per request")
	flags.StringVar(&cfg.format, "format", "png", "output format: png or svg")
	flags.StringVar(&diagramFile, "diagram-file", "", "Mermaid source file; default is a small graph")
	flags.BoolVar(&cfg.unique, "unique", false, "add a unique Mermaid comment to each request")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	cfg.format = strings.ToLower(strings.TrimSpace(cfg.format))
	cfg.diagram = []byte(defaultDiagram)
	if diagramFile != "" {
		diagram, err := os.ReadFile(diagramFile)
		if err != nil {
			return config{}, fmt.Errorf("read diagram file: %w", err)
		}
		cfg.diagram = diagram
	}
	if err := validateConfig(&cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg *config) error {
	if cfg.total <= 0 {
		return errors.New("requests must be positive")
	}
	if cfg.total > maxRequests {
		return fmt.Errorf("requests must not exceed %d", maxRequests)
	}
	if cfg.concurrency <= 0 {
		return errors.New("concurrency must be positive")
	}
	if cfg.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if cfg.format != "png" && cfg.format != "svg" {
		return errors.New("format must be png or svg")
	}
	if len(bytes.TrimSpace(cfg.diagram)) == 0 {
		return errors.New("diagram must not be empty")
	}

	target, err := renderTarget(cfg.target, cfg.format)
	if err != nil {
		return err
	}
	cfg.target = target
	return nil
}

func renderTarget(rawTarget, format string) (string, error) {
	target, err := url.Parse(rawTarget)
	if err != nil {
		return "", fmt.Errorf("invalid target URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", errors.New("target URL scheme must be http or https")
	}
	if target.Host == "" {
		return "", errors.New("target URL must include a host")
	}
	if target.Fragment != "" {
		return "", errors.New("target URL must not include a fragment")
	}

	path := strings.TrimSuffix(target.Path, "/")
	switch {
	case path == "":
		target.Path = "/render"
	case strings.HasSuffix(path, "/render"):
		target.Path = path
	default:
		return "", errors.New("target URL path must be /render or end in /render")
	}
	query := target.Query()
	query.Set("format", format)
	target.RawQuery = query.Encode()
	return target.String(), nil
}

func newHTTPClient(concurrency int) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = concurrency
	transport.MaxIdleConnsPerHost = concurrency
	transport.MaxConnsPerHost = concurrency
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func execute(ctx context.Context, client *http.Client, cfg config) report {
	started := time.Now()
	workerCount := min(cfg.concurrency, cfg.total)
	jobs := make(chan int, workerCount)
	results := make(chan requestResult, workerCount)

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					results <- performRequest(ctx, client, cfg, index)
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for index := 0; index < cfg.total; index++ {
			select {
			case <-ctx.Done():
				return
			case jobs <- index:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	latencies := make([]time.Duration, 0, cfg.total)
	result := report{statusCodes: make(map[int]int)}
	for request := range results {
		result.total++
		result.statusCodes[request.statusCode]++
		result.bytes += request.bytes
		latencies = append(latencies, request.latency)
		if request.failed {
			result.errors++
		} else {
			result.success++
		}
	}

	result.elapsed = time.Since(started)
	if result.elapsed > 0 {
		result.requestsPerSecond = float64(result.total) / result.elapsed.Seconds()
	}
	result.p50, result.p95, result.p99, result.maxLatency = latencyPercentiles(latencies)
	return result
}

func performRequest(ctx context.Context, client *http.Client, cfg config, index int) requestResult {
	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	body := requestDiagram(cfg.diagram, index, cfg.unique)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.target, bytes.NewReader(body))
	if err != nil {
		return requestResult{latency: time.Since(started), failed: true}
	}
	request.Header.Set("Content-Type", "text/plain")
	if cfg.format == "svg" {
		request.Header.Set("Accept", "image/svg+xml")
	} else {
		request.Header.Set("Accept", "image/png")
	}

	response, err := client.Do(request)
	if err != nil {
		return requestResult{latency: time.Since(started), failed: true}
	}

	bodyBytes, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	failed := response.StatusCode != http.StatusOK || !hasExpectedMIME(response.Header.Get("Content-Type"), cfg.format)
	if readErr != nil || closeErr != nil {
		failed = true
	}
	return requestResult{
		latency:    time.Since(started),
		bytes:      bodyBytes,
		statusCode: response.StatusCode,
		failed:     failed,
	}
}

func requestDiagram(diagram []byte, index int, unique bool) []byte {
	if !unique {
		return diagram
	}

	result := make([]byte, 0, len(diagram)+40)
	result = append(result, diagram...)
	if len(result) > 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, "%% loadtest request "...)
	result = strconv.AppendInt(result, int64(index), 10)
	result = append(result, '\n')
	return result
}

func hasExpectedMIME(contentType, format string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	expected := "image/png"
	if format == "svg" {
		expected = "image/svg+xml"
	}
	return strings.EqualFold(mediaType, expected)
}

func latencyPercentiles(samples []time.Duration) (p50, p95, p99, maxLatency time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return nearestRank(sorted, 50), nearestRank(sorted, 95), nearestRank(sorted, 99), sorted[len(sorted)-1]
}

func nearestRank(sorted []time.Duration, percentile int) time.Duration {
	rank := (percentile*len(sorted) + 99) / 100
	return sorted[rank-1]
}

func toJSONReport(result report) jsonReport {
	return jsonReport{
		Total:             result.total,
		Success:           result.success,
		Errors:            result.errors,
		Elapsed:           result.elapsed.String(),
		RequestsPerSecond: float64(int(result.requestsPerSecond*100+0.5)) / 100,
		Bytes:             result.bytes,
		P50:               result.p50.String(),
		P95:               result.p95.String(),
		P99:               result.p99.String(),
		MaxLatency:        result.maxLatency.String(),
		StatusCodes:       result.statusCodes,
	}
}
