package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewCacheRejectsInvalidConfig(t *testing.T) {
	valid := CacheConfig{
		BundleID:              "mermaid-11/chromium-1/fonts-1",
		MaxBytes:              1,
		MaxEntries:            1,
		MaxOutputBytes:        1,
		TTL:                   time.Second,
		RejectionTTL:          20 * time.Second,
		FillTimeout:           time.Second,
		MaxWaiters:            1_024,
		MaxConcurrentFills:    1,
		MaxQueuedFills:        0,
		MissesPerSecond:       1,
		MissBurst:             1,
		ClientMissesPerSecond: 1,
		ClientMissBurst:       1,
		MaxClients:            1,
	}

	tests := []struct {
		name   string
		inner  Renderer
		change func(*CacheConfig)
	}{
		{name: "nil renderer", inner: nil},
		{name: "missing bundle ID", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.BundleID = "" }},
		{name: "invalid byte limit", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MaxBytes = 0 }},
		{name: "invalid entry limit", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MaxEntries = 0 }},
		{name: "invalid output limit", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MaxOutputBytes = 0 }},
		{name: "invalid TTL", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.TTL = 0 }},
		{name: "invalid concurrency", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MaxConcurrentFills = 0 }},
		{name: "invalid queue", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MaxQueuedFills = -1 }},
		{name: "invalid miss rate", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MissesPerSecond = 0 }},
		{name: "invalid miss burst", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MissBurst = 0 }},
		{name: "invalid client miss rate", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.ClientMissesPerSecond = 0 }},
		{name: "invalid client miss burst", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.ClientMissBurst = 0 }},
		{name: "invalid client count", inner: rendererFunc(successfulRender), change: func(c *CacheConfig) { c.MaxClients = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := valid
			if tt.change != nil {
				tt.change(&config)
			}
			if _, err := NewCache(tt.inner, config); err == nil {
				t.Fatal("NewCache() error = nil")
			}
		})
	}
}

func TestContentKeyIsStableAndIncludesResolvedInputs(t *testing.T) {
	key := ContentKey("bundle-v1", Request{Diagram: "graph TD; A-->B"})
	if want := "e67e3cfba9b8b7f8838b23a60e9aa5636e18d1f9e812e704e4fe0533bd9364ee"; key != want {
		t.Fatalf("ContentKey() = %q, want %q", key, want)
	}

	pngKey := ContentKey("bundle-v1", Request{Diagram: "graph TD; A-->B", Format: FormatPNG})
	if key != pngKey {
		t.Fatal("default format key differs from resolved PNG key")
	}
	if got := ContentKey("bundle-v1", Request{Diagram: "graph TD; A-->B", Format: Format(" PNG ")}); got != pngKey {
		t.Fatalf("normalized PNG key = %q, want %q", got, pngKey)
	}

	variants := []Request{
		{Diagram: "graph TD; A-->B ", Format: FormatPNG},
		{Diagram: "graph TD; A-->B", Format: FormatSVG},
	}
	for _, request := range variants {
		variant := ContentKey("bundle-v1", request)
		if variant == key {
			t.Fatalf("ContentKey(%+v) did not include exact source and format", request)
		}
	}
	otherBundle := ContentKey("bundle-v2", Request{Diagram: "graph TD; A-->B", Format: FormatPNG})
	if otherBundle == key {
		t.Fatal("ContentKey() did not include renderer bundle ID")
	}
	left := ContentKey("a", Request{Diagram: "d", Format: Format("bc")})
	right := ContentKey("ab", Request{Diagram: "d", Format: Format("c")})
	if left == right {
		t.Fatal("ContentKey() framing is ambiguous")
	}
}
func TestCacheHitClosesInnerResultAndReturnsIndependentReaders(t *testing.T) {
	var calls atomic.Int64
	var closes atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		data := encodedImage(request.Format, "rendered:"+string(request.Format)+":"+request.Diagram)
		return Result{
			ReadCloser: &countingReadCloser{Reader: bytes.NewReader(data), closes: &closes},
			MIMEType:   request.Format.MIMEType(),
			Size:       int64(len(data)),
		}, nil
	})
	cache := mustCache(t, inner, CacheConfig{
		BundleID:              "bundle-v1",
		MaxBytes:              1_024,
		MaxEntries:            1_000,
		MaxOutputBytes:        1_024,
		TTL:                   time.Minute,
		RejectionTTL:          20 * time.Second,
		FillTimeout:           time.Second,
		MaxWaiters:            1_024,
		MaxConcurrentFills:    1,
		MaxQueuedFills:        0,
		MissesPerSecond:       100,
		MissBurst:             100,
		ClientMissesPerSecond: 100,
		ClientMissBurst:       100,
		MaxClients:            100,
	})
	request := Request{Diagram: "graph TD; A-->B", Format: FormatSVG}

	first := renderBytes(t, cache, request)
	if got, want := imagePayload(FormatSVG, first), "rendered:svg:graph TD; A-->B"; got != want {
		t.Fatalf("first payload = %q, want %q", got, want)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("inner closes = %d, want 1 before returning cached result", got)
	}

	second, err := cache.Render(context.Background(), request)
	if err != nil {
		t.Fatalf("second Render() error = %v", err)
	}
	third, err := cache.Render(context.Background(), request)
	if err != nil {
		t.Fatalf("third Render() error = %v", err)
	}
	secondData, err := io.ReadAll(second)
	if err != nil {
		t.Fatalf("read second result: %v", err)
	}
	thirdData, err := io.ReadAll(third)
	if err != nil {
		t.Fatalf("read third result: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second result: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close third result: %v", err)
	}
	if string(secondData) != string(first) || string(thirdData) != string(first) {
		t.Fatalf("cached readers returned %q and %q, want %q", secondData, thirdData, first)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("inner calls = %d, want 1", got)
	}
	stats := cache.Stats()
	if stats.Hits != 2 || stats.Misses != 1 || stats.Bytes != int64(len(first)) || stats.ActiveFills != 0 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCacheCoalescesSameKeyBurst(t *testing.T) {
	const callers = 32
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var calls atomic.Int64
	inner := rendererFunc(func(ctx context.Context, request Request) (Result, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
			return resultFor(request, request.Diagram), nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	})
	cache := mustCache(t, inner, CacheConfig{
		BundleID:              "bundle-v1",
		MaxBytes:              1_024,
		MaxEntries:            1_000,
		MaxOutputBytes:        1_024,
		TTL:                   time.Minute,
		RejectionTTL:          20 * time.Second,
		FillTimeout:           time.Second,
		MaxWaiters:            1_024,
		MaxConcurrentFills:    1,
		MaxQueuedFills:        0,
		MissesPerSecond:       1,
		MissBurst:             1,
		ClientMissesPerSecond: 100,
		ClientMissBurst:       100,
		MaxClients:            100,
	})

	start := make(chan struct{})
	errCh := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			result, err := cache.Render(context.Background(), Request{Diagram: "same", Format: FormatPNG})
			if err == nil {
				var data []byte
				data, err = io.ReadAll(result)
				if closeErr := result.Close(); err == nil {
					err = closeErr
				}
				if err == nil && imagePayload(FormatPNG, data) != "same" {
					err = errors.New("unexpected rendered data")
				}
			}
			errCh <- err
		}()
	}
	close(start)
	awaitClosed(t, started, "inner render start")
	awaitCondition(t, func() bool { return cache.Stats().Coalesced == callers-1 }, "all callers to coalesce")
	close(release)
	for range callers {
		if err := <-errCh; err != nil {
			t.Fatalf("coalesced Render() error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("inner calls = %d, want 1", got)
	}
	stats := cache.Stats()
	if stats.Misses != callers || stats.Coalesced != callers-1 || stats.Fills != 1 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCacheSeparatesFormats(t *testing.T) {
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		return resultFor(request, string(request.Format)), nil
	})
	cache := mustCache(t, inner, defaultCacheConfig())
	request := Request{Diagram: "same"}

	if got := imagePayload(request.Format, renderBytes(t, cache, request)); got != "png" {
		t.Fatalf("default render = %q, want png", got)
	}
	request.Format = FormatSVG
	if got := imagePayload(request.Format, renderBytes(t, cache, request)); got != "svg" {
		t.Fatalf("SVG render = %q, want svg", got)
	}
	request.Format = FormatPNG
	if got := imagePayload(request.Format, renderBytes(t, cache, request)); got != "png" {
		t.Fatalf("PNG cache hit = %q, want png", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("inner calls = %d, want 2", got)
	}
}

func TestCacheEvictsLeastRecentlyUsedBytes(t *testing.T) {
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	entrySize := int64(len(encodedImage(FormatPNG, "aaa")))
	config.MaxBytes = 2 * entrySize
	cache := mustCache(t, inner, config)

	renderBytes(t, cache, Request{Diagram: "aaa", Format: FormatPNG})
	renderBytes(t, cache, Request{Diagram: "bbb", Format: FormatPNG})
	renderBytes(t, cache, Request{Diagram: "aaa", Format: FormatPNG})
	renderBytes(t, cache, Request{Diagram: "ccc", Format: FormatPNG})
	stats := cache.Stats()
	if stats.Evictions != 1 || stats.Bytes != 2*entrySize || stats.Entries != 2 {
		t.Fatalf("Stats() after eviction = %+v", stats)
	}

	renderBytes(t, cache, Request{Diagram: "aaa", Format: FormatPNG})
	if got := calls.Load(); got != 3 {
		t.Fatalf("inner calls after retained LRU hit = %d, want 3", got)
	}
	renderBytes(t, cache, Request{Diagram: "bbb", Format: FormatPNG})
	if got := calls.Load(); got != 4 {
		t.Fatalf("inner calls after evicted key = %d, want 4", got)
	}
}

func TestCacheEvictsLeastRecentlyUsedEntryCount(t *testing.T) {
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	config.MaxEntries = 2
	cache := mustCache(t, inner, config)

	renderBytes(t, cache, Request{Diagram: "a", Format: FormatSVG})
	renderBytes(t, cache, Request{Diagram: "b", Format: FormatSVG})
	renderBytes(t, cache, Request{Diagram: "a", Format: FormatSVG})
	renderBytes(t, cache, Request{Diagram: "c", Format: FormatSVG})
	stats := cache.Stats()
	if stats.Evictions != 1 || stats.Entries != 2 {
		t.Fatalf("Stats() after entry eviction = %+v", stats)
	}

	renderBytes(t, cache, Request{Diagram: "a", Format: FormatSVG})
	if got := calls.Load(); got != 3 {
		t.Fatalf("inner calls after retained LRU hit = %d, want 3", got)
	}
	renderBytes(t, cache, Request{Diagram: "b", Format: FormatSVG})
	if got := calls.Load(); got != 4 {
		t.Fatalf("inner calls after entry-count eviction = %d, want 4", got)
	}
}

func TestCacheDoesNotStoreEntryLargerThanByteLimit(t *testing.T) {
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	config.MaxBytes = int64(len(encodedImage(FormatPNG, "large"))) - 1
	cache := mustCache(t, inner, config)
	request := Request{Diagram: "large", Format: FormatPNG}

	renderBytes(t, cache, request)
	renderBytes(t, cache, request)
	if got := calls.Load(); got != 2 {
		t.Fatalf("inner calls = %d, want 2", got)
	}
	stats := cache.Stats()
	if stats.Bytes != 0 || stats.Entries != 0 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCacheExpiresEntriesAtTTL(t *testing.T) {
	clock := newFakeClock(time.Unix(1_000, 0))
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	config.TTL = time.Minute
	config.Clock = clock.Now
	cache := mustCache(t, inner, config)
	request := Request{Diagram: "ttl", Format: FormatPNG}

	renderBytes(t, cache, request)
	clock.Advance(time.Minute - time.Nanosecond)
	renderBytes(t, cache, request)
	if got := calls.Load(); got != 1 {
		t.Fatalf("inner calls before TTL = %d, want 1", got)
	}
	clock.Advance(time.Nanosecond)
	renderBytes(t, cache, request)
	if got := calls.Load(); got != 2 {
		t.Fatalf("inner calls at TTL = %d, want 2", got)
	}
	if got, want := cache.Stats().Bytes, int64(len(encodedImage(FormatPNG, "ttl"))); got != want {
		t.Fatalf("cached bytes = %d, want %d", got, want)
	}
}

func TestCanceledWaiterDoesNotCancelOtherWaiters(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	innerCanceled := make(chan struct{})
	var once sync.Once
	inner := rendererFunc(func(ctx context.Context, _ Request) (Result, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return resultFor(Request{Format: FormatPNG}, "shared"), nil
		case <-ctx.Done():
			close(innerCanceled)
			return Result{}, ctx.Err()
		}
	})
	cache := mustCache(t, inner, defaultCacheConfig())
	request := Request{Diagram: "shared", Format: FormatPNG}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := cache.Render(firstCtx, request)
		firstErr <- err
	}()
	awaitClosed(t, started, "inner render start")
	secondResult := make(chan []byte, 1)
	secondErr := make(chan error, 1)
	go func() {
		result, err := cache.Render(context.Background(), request)
		if err != nil {
			secondErr <- err
			return
		}
		data, readErr := io.ReadAll(result)
		closeErr := result.Close()
		secondResult <- data
		secondErr <- errors.Join(readErr, closeErr)
	}()
	awaitCondition(t, func() bool { return cache.Stats().Coalesced == 1 }, "second waiter to coalesce")

	cancelFirst()
	assertRenderErrorCode(t, <-firstErr, CodeCanceled)
	select {
	case <-innerCanceled:
		t.Fatal("one canceled waiter canceled the shared fill")
	default:
	}
	close(release)
	if err := <-secondErr; err != nil {
		t.Fatalf("second Render() error = %v", err)
	}
	if got := imagePayload(FormatPNG, <-secondResult); got != "shared" {
		t.Fatalf("second result = %q, want shared", got)
	}
}

func TestLastCanceledWaiterCancelsFill(t *testing.T) {
	started := make(chan struct{})
	innerCanceled := make(chan struct{})
	inner := rendererFunc(func(ctx context.Context, _ Request) (Result, error) {
		close(started)
		<-ctx.Done()
		close(innerCanceled)
		return Result{}, ctx.Err()
	})
	cache := mustCache(t, inner, defaultCacheConfig())
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := cache.Render(ctx, Request{Diagram: "cancel", Format: FormatPNG})
		errCh <- err
	}()
	awaitClosed(t, started, "inner render start")

	cancel()
	assertRenderErrorCode(t, <-errCh, CodeCanceled)
	awaitClosed(t, innerCanceled, "inner fill cancellation")
	awaitCondition(t, func() bool { return cache.Stats().ActiveFills == 0 }, "active fill count to clear")
}

func TestCacheBoundsConcurrentAndQueuedFillsWhileServingHits(t *testing.T) {
	startedA := make(chan struct{})
	startedB := make(chan struct{})
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	inner := rendererFunc(func(ctx context.Context, request Request) (Result, error) {
		var started chan struct{}
		var release chan struct{}
		switch request.Diagram {
		case "hot":
			return resultFor(request, "hot"), nil
		case "a":
			started, release = startedA, releaseA
		case "b":
			started, release = startedB, releaseB
		default:
			return resultFor(request, request.Diagram), nil
		}
		close(started)
		select {
		case <-release:
			return resultFor(request, request.Diagram), nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	})
	config := defaultCacheConfig()
	config.MaxConcurrentFills = 1
	config.MaxQueuedFills = 1
	cache := mustCache(t, inner, config)
	renderBytes(t, cache, Request{Diagram: "hot", Format: FormatPNG})

	aErr := make(chan error, 1)
	go func() {
		_, err := cache.Render(context.Background(), Request{Diagram: "a", Format: FormatPNG})
		aErr <- err
	}()
	awaitClosed(t, startedA, "first fill start")
	if got := cache.Stats().ActiveFills; got != 1 {
		t.Fatalf("active fills = %d, want 1", got)
	}

	bErr := make(chan error, 1)
	go func() {
		_, err := cache.Render(context.Background(), Request{Diagram: "b", Format: FormatPNG})
		bErr <- err
	}()
	awaitCondition(t, func() bool { return cache.Stats().Misses >= 3 }, "queued fill admission")
	select {
	case <-startedB:
		t.Fatal("queued fill started before a render slot was free")
	default:
	}

	if got := imagePayload(FormatPNG, renderBytes(t, cache, Request{Diagram: "hot", Format: FormatPNG})); got != "hot" {
		t.Fatalf("cache hit while saturated = %q, want hot", got)
	}
	_, err := cache.Render(context.Background(), Request{Diagram: "c", Format: FormatPNG})
	assertRenderErrorCode(t, err, CodeOverloaded)

	close(releaseA)
	if err := <-aErr; err != nil {
		t.Fatalf("first fill error = %v", err)
	}
	awaitClosed(t, startedB, "queued fill start")
	close(releaseB)
	if err := <-bErr; err != nil {
		t.Fatalf("queued fill error = %v", err)
	}
	stats := cache.Stats()
	if stats.Overloads != 1 || stats.CapacityOverloads != 1 || stats.ActiveFills != 0 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCacheRateLimitsOnlyNewMisses(t *testing.T) {
	clock := newFakeClock(time.Unix(2_000, 0))
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	config.Clock = clock.Now
	config.MissesPerSecond = 1
	config.MissBurst = 1
	cache := mustCache(t, inner, config)

	renderBytes(t, cache, Request{Diagram: "a", Format: FormatPNG})
	_, err := cache.Render(context.Background(), Request{Diagram: "b", Format: FormatPNG})
	assertRenderErrorCode(t, err, CodeOverloaded)
	if got := imagePayload(FormatPNG, renderBytes(t, cache, Request{Diagram: "a", Format: FormatPNG})); got != "a" {
		t.Fatalf("hit under empty rate bucket = %q, want a", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("inner calls before refill = %d, want 1", got)
	}

	clock.Advance(time.Second)
	renderBytes(t, cache, Request{Diagram: "b", Format: FormatPNG})
	if got := calls.Load(); got != 2 {
		t.Fatalf("inner calls after refill = %d, want 2", got)
	}
	stats := cache.Stats()
	if stats.Overloads != 1 || stats.RateOverloads != 1 || stats.Hits != 1 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCacheRateLimitsUniqueMissesPerClientWithoutBlockingHits(t *testing.T) {
	clock := newFakeClock(time.Unix(2_000, 0))
	config := defaultCacheConfig()
	config.Clock = clock.Now
	config.ClientMissesPerSecond = 1
	config.ClientMissBurst = 1
	cache := mustCache(t, rendererFunc(successfulRender), config)
	clientA := WithClientIdentity(context.Background(), "client-a")
	clientB := WithClientIdentity(context.Background(), "client-b")

	renderBytesWithContext(t, clientA, cache, Request{Diagram: "a", Format: FormatPNG})
	_, err := cache.Render(clientA, Request{Diagram: "b", Format: FormatPNG})
	assertRenderErrorCode(t, err, CodeOverloaded)
	if got := imagePayload(FormatPNG, renderBytesWithContext(t, clientA, cache, Request{Diagram: "a", Format: FormatPNG})); got != "a" {
		t.Fatalf("cache hit = %q, want a", got)
	}
	renderBytesWithContext(t, clientB, cache, Request{Diagram: "b", Format: FormatPNG})
}

func TestCacheAppliesClusterAdmissionOnlyToUniqueFills(t *testing.T) {
	var allow atomic.Bool
	allow.Store(true)
	var admissions atomic.Int64
	config := defaultCacheConfig()
	config.MissAdmitter = admitterFunc(func(context.Context) (bool, error) {
		admissions.Add(1)
		return allow.Load(), nil
	})
	cache := mustCache(t, rendererFunc(successfulRender), config)

	renderBytes(t, cache, Request{Diagram: "cached", Format: FormatSVG})
	allow.Store(false)
	renderBytes(t, cache, Request{Diagram: "cached", Format: FormatSVG})
	_, err := cache.Render(context.Background(), Request{Diagram: "new", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeOverloaded)
	if got := admissions.Load(); got != 2 {
		t.Fatalf("admissions = %d, want 2", got)
	}
	stats := cache.Stats()
	if stats.ClusterOverloads != 1 || stats.Overloads != 1 || stats.Fills != 1 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCacheFailsClosedWhenClusterAdmissionErrors(t *testing.T) {
	config := defaultCacheConfig()
	config.MissAdmitter = admitterFunc(func(context.Context) (bool, error) {
		return false, errors.New("unavailable")
	})
	cache := mustCache(t, rendererFunc(successfulRender), config)
	_, err := cache.Render(context.Background(), Request{Diagram: "new", Format: FormatSVG})
	assertRenderErrorCode(t, err, CodeOverloaded)
}

func TestCacheValidatesOutputBeforeCaching(t *testing.T) {
	tests := []struct {
		name      string
		format    Format
		data      []byte
		maxOutput int64
		wantCode  ErrorCode
		cacheable bool
	}{
		{
			name:      "invalid PNG",
			format:    FormatPNG,
			data:      []byte("not a PNG"),
			maxOutput: 100,
			wantCode:  CodeInternal,
		},
		{
			name:      "unsafe SVG",
			format:    FormatSVG,
			data:      []byte("<svg><script>bad()</script></svg>"),
			maxOutput: 100,
			wantCode:  CodeRenderRejected,
			cacheable: true,
		},
		{
			name:      "oversized",
			format:    FormatPNG,
			data:      encodedImage(FormatPNG, "large"),
			maxOutput: 4,
			wantCode:  CodeTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			var closes atomic.Int64
			inner := rendererFunc(func(_ context.Context, _ Request) (Result, error) {
				calls.Add(1)
				return Result{
					ReadCloser: &countingReadCloser{Reader: bytes.NewReader(tt.data), closes: &closes},
					MIMEType:   tt.format.MIMEType(),
					Size:       int64(len(tt.data)),
				}, nil
			})
			config := defaultCacheConfig()
			config.MaxOutputBytes = tt.maxOutput
			cache := mustCache(t, inner, config)
			request := Request{Diagram: tt.name, Format: tt.format}

			for range 2 {
				_, err := cache.Render(context.Background(), request)
				assertRenderErrorCode(t, err, tt.wantCode)
			}
			wantCalls := int64(2)
			wantEntries := int64(0)
			if tt.cacheable {
				wantCalls = 1
				wantEntries = 1
			}
			if got := calls.Load(); got != wantCalls {
				t.Fatalf("inner calls = %d, want %d", got, wantCalls)
			}
			if got := closes.Load(); got != wantCalls {
				t.Fatalf("inner closes = %d, want %d", got, wantCalls)
			}
			if stats := cache.Stats(); stats.Bytes != 0 || stats.Entries != wantEntries {
				t.Fatalf("Stats() = %+v", stats)
			}
		})
	}
}

func TestCacheCachesDeterministicRejections(t *testing.T) {
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, _ Request) (Result, error) {
		calls.Add(1)
		return Result{}, &Error{Code: CodeRenderRejected, Message: "bad diagram"}
	})
	cache := mustCache(t, inner, defaultCacheConfig())
	request := Request{Diagram: "bad", Format: FormatPNG}

	for range 2 {
		_, err := cache.Render(context.Background(), request)
		assertRenderErrorCode(t, err, CodeRenderRejected)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("inner calls = %d, want 1", got)
	}
	if stats := cache.Stats(); stats.Bytes != 0 || stats.Entries != 1 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestCachedRejectionExpires(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, _ Request) (Result, error) {
		calls.Add(1)
		return Result{}, &Error{Code: CodeRenderRejected, Message: "bad diagram"}
	})
	config := defaultCacheConfig()
	config.Clock = clock.Now
	config.RejectionTTL = 20 * time.Second
	cache := mustCache(t, inner, config)
	request := Request{Diagram: "bad", Format: FormatSVG}

	for range 2 {
		_, err := cache.Render(context.Background(), request)
		assertRenderErrorCode(t, err, CodeRenderRejected)
	}
	clock.Advance(20 * time.Second)
	_, err := cache.Render(context.Background(), request)
	assertRenderErrorCode(t, err, CodeRenderRejected)
	if calls.Load() != 2 {
		t.Fatalf("inner calls = %d, want 2", calls.Load())
	}
}

func TestCacheFillTimeoutIncludesQueueWait(t *testing.T) {
	var calls atomic.Int64
	inner := rendererFunc(func(_ context.Context, request Request) (Result, error) {
		calls.Add(1)
		<-time.After(75 * time.Millisecond)
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	config.MaxConcurrentFills = 1
	config.MaxQueuedFills = 1
	config.FillTimeout = 50 * time.Millisecond
	cache := mustCache(t, inner, config)

	errors := make(chan error, 2)
	for _, diagram := range []string{"active", "queued"} {
		go func() {
			_, err := cache.Render(context.Background(), Request{Diagram: diagram, Format: FormatPNG})
			errors <- err
		}()
	}
	for range 2 {
		assertRenderErrorCode(t, <-errors, CodeTimeout)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner calls = %d, want only active fill", calls.Load())
	}
}

func TestCacheHitsBypassMissWaiterCapacity(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	inner := rendererFunc(func(ctx context.Context, request Request) (Result, error) {
		if request.Diagram == "blocked" {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return Result{}, contextError(ctx.Err())
			}
		}
		return resultFor(request, request.Diagram), nil
	})
	config := defaultCacheConfig()
	config.MaxWaiters = 1
	cache := mustCache(t, inner, config)
	cached := Request{Diagram: "cached", Format: FormatPNG}
	renderBytes(t, cache, cached)

	blockedDone := make(chan error, 1)
	go func() {
		result, err := cache.Render(context.Background(), Request{Diagram: "blocked", Format: FormatPNG})
		if result.ReadCloser != nil {
			_ = result.Close()
		}
		blockedDone <- err
	}()
	<-started
	if got := imagePayload(FormatPNG, renderBytes(t, cache, cached)); got != "cached" {
		t.Fatalf("cache hit payload = %q", got)
	}
	_, err := cache.Render(context.Background(), Request{Diagram: "other", Format: FormatPNG})
	assertRenderErrorCode(t, err, CodeOverloaded)
	close(release)
	if err := <-blockedDone; err != nil {
		t.Fatalf("blocked render error = %v", err)
	}
}

func TestCacheReadOrCloseFailureIsNotCached(t *testing.T) {
	tests := []struct {
		name   string
		reader func() io.ReadCloser
	}{
		{name: "read", reader: func() io.ReadCloser { return &failingReadCloser{readErr: errors.New("read failed")} }},
		{name: "close", reader: func() io.ReadCloser { return &failingReadCloser{data: "data", closeErr: errors.New("close failed")} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			inner := rendererFunc(func(_ context.Context, _ Request) (Result, error) {
				calls.Add(1)
				return Result{ReadCloser: tt.reader(), MIMEType: "image/png", Size: 4}, nil
			})
			cache := mustCache(t, inner, defaultCacheConfig())
			request := Request{Diagram: "failure", Format: FormatPNG}
			for range 2 {
				_, err := cache.Render(context.Background(), request)
				assertRenderErrorCode(t, err, CodeInternal)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("inner calls = %d, want 2", got)
			}
		})
	}
}

type admitterFunc func(context.Context) (bool, error)

func (f admitterFunc) Admit(ctx context.Context) (bool, error) {
	return f(ctx)
}

type rendererFunc func(context.Context, Request) (Result, error)

func (f rendererFunc) Render(ctx context.Context, request Request) (Result, error) {
	return f(ctx, request)
}

func successfulRender(_ context.Context, request Request) (Result, error) {
	return resultFor(request, request.Diagram), nil
}

func resultFor(request Request, payload string) Result {
	data := encodedImage(request.Format, payload)
	return Result{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		MIMEType:   request.Format.MIMEType(),
		Size:       int64(len(data)),
	}
}

func encodedImage(format Format, payload string) []byte {
	if format == FormatSVG {
		return []byte("<svg><text>" + payload + "</text></svg>")
	}
	base := testPNG(1, 1)
	result := append([]byte(nil), base[:len(base)-12]...)
	result = appendPNGChunk(result, "tEXt", []byte(payload))
	return append(result, base[len(base)-12:]...)
}

func imagePayload(format Format, data []byte) string {
	if format == FormatSVG {
		return strings.TrimSuffix(strings.TrimPrefix(string(data), "<svg><text>"), "</text></svg>")
	}
	index := bytes.Index(data, []byte("tEXt"))
	if index < 4 {
		return ""
	}
	length := int(binary.BigEndian.Uint32(data[index-4 : index]))
	return string(data[index+4 : index+4+length])
}

func defaultCacheConfig() CacheConfig {
	return CacheConfig{
		BundleID:              "bundle-v1",
		MaxBytes:              1_024,
		MaxEntries:            1_000,
		MaxOutputBytes:        1_024,
		TTL:                   time.Minute,
		RejectionTTL:          20 * time.Second,
		FillTimeout:           time.Second,
		MaxWaiters:            1_024,
		MaxConcurrentFills:    4,
		MaxQueuedFills:        4,
		MissesPerSecond:       1_000,
		MissBurst:             1_000,
		ClientMissesPerSecond: 1_000,
		ClientMissBurst:       1_000,
		MaxClients:            1_000,
	}
}

func mustCache(t *testing.T, inner Renderer, config CacheConfig) *Cache {
	t.Helper()
	cache, err := NewCache(inner, config)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	return cache
}

func renderBytes(t *testing.T, renderer Renderer, request Request) []byte {
	t.Helper()
	return renderBytesWithContext(t, context.Background(), renderer, request)
}

func renderBytesWithContext(t *testing.T, ctx context.Context, renderer Renderer, request Request) []byte {
	t.Helper()
	result, err := renderer.Render(ctx, request)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	data, readErr := io.ReadAll(result)
	closeErr := result.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("consume result: %v", err)
	}
	if result.Size != int64(len(data)) {
		t.Fatalf("Result.Size = %d, want %d", result.Size, len(data))
	}
	return data
}

func assertRenderErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", want)
	}
	var renderErr *Error
	if !errors.As(err, &renderErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if renderErr.Code != want {
		t.Fatalf("error code = %q, want %q (error: %v)", renderErr.Code, want, err)
	}
}

func awaitClosed(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

type countingReadCloser struct {
	io.Reader
	closes *atomic.Int64
}

func (r *countingReadCloser) Close() error {
	r.closes.Add(1)
	return nil
}

type failingReadCloser struct {
	data     string
	readErr  error
	closeErr error
	read     bool
}

func (r *failingReadCloser) Read(buffer []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	if r.readErr != nil {
		return 0, r.readErr
	}
	copy(buffer, r.data)
	return len(r.data), io.EOF
}

func (r *failingReadCloser) Close() error {
	return r.closeErr
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}
