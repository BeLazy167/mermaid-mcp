package render

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const cacheKeyDomain = "mermaid-mcp/render-cache/v1"

// MissAdmitter applies an optional cluster-wide budget before unique work begins.
type MissAdmitter interface {
	Admit(context.Context) (bool, error)
}

// CacheConfig configures a cache-first renderer and its render-miss admission.
type CacheConfig struct {
	BundleID              string
	MaxBytes              int64
	MaxEntries            int
	MaxOutputBytes        int64
	TTL                   time.Duration
	RejectionTTL          time.Duration
	FillTimeout           time.Duration
	MaxWaiters            int
	MaxConcurrentFills    int
	MaxQueuedFills        int
	MissesPerSecond       float64
	MissBurst             int
	ClientMissesPerSecond float64
	ClientMissBurst       int
	MaxClients            int
	MissAdmitter          MissAdmitter

	// Clock overrides time.Now. It is useful for deterministic expiry and rate tests.
	Clock func() time.Time
}

// CacheStats is an atomic snapshot of cache and render-miss activity.
type CacheStats struct {
	Hits              uint64
	Misses            uint64
	Coalesced         uint64
	Overloads         uint64
	CapacityOverloads uint64
	RateOverloads     uint64
	ClusterOverloads  uint64
	Evictions         uint64
	Expirations       uint64
	Fills             uint64
	Bytes             int64
	Entries           int64
	PendingFills      int64
	ActiveFills       int64
}

// Cache wraps a Renderer with a byte- and entry-bounded LRU plus bounded miss admission.
type Cache struct {
	inner                 Renderer
	rendererBundleID      string
	maxBytes              int64
	maxEntries            int
	maxOutputBytes        int64
	ttl                   time.Duration
	rejectionTTL          time.Duration
	fillTimeout           time.Duration
	maxPendingFills       int
	missesPerSecond       float64
	missBurst             float64
	clientMissesPerSecond float64
	clientMissBurst       float64
	maxClients            int
	missAdmitter          MissAdmitter
	now                   func() time.Time

	mu            sync.Mutex
	entries       map[[sha256.Size]byte]*list.Element
	lru           list.List
	inflight      map[[sha256.Size]byte]*cacheFill
	clientBuckets map[[sha256.Size]byte]*list.Element
	clientLRU     list.List
	pendingFills  int
	tokens        float64
	lastRefill    time.Time

	fillSlots   chan struct{}
	waiterSlots chan struct{}
	counters    cacheCounters
}

type cacheCounters struct {
	hits              atomic.Uint64
	misses            atomic.Uint64
	coalesced         atomic.Uint64
	overloads         atomic.Uint64
	capacityOverloads atomic.Uint64
	rateOverloads     atomic.Uint64
	clusterOverloads  atomic.Uint64
	evictions         atomic.Uint64
	expirations       atomic.Uint64
	fills             atomic.Uint64
	bytes             atomic.Int64
	entries           atomic.Int64
	pendingFills      atomic.Int64
	activeFills       atomic.Int64
}

type clientBucket struct {
	key        [sha256.Size]byte
	tokens     float64
	lastRefill time.Time
}

type clientIdentityKey struct{}

// WithClientIdentity attaches a stable client identity for per-client miss admission.
func WithClientIdentity(ctx context.Context, identity string) context.Context {
	if identity == "" {
		identity = "unknown"
	}
	return context.WithValue(ctx, clientIdentityKey{}, sha256.Sum256([]byte(identity)))
}

func clientIdentity(ctx context.Context) [sha256.Size]byte {
	if key, ok := ctx.Value(clientIdentityKey{}).([sha256.Size]byte); ok {
		return key
	}
	return sha256.Sum256([]byte("unknown"))
}

type cacheEntry struct {
	key       [sha256.Size]byte
	data      []byte
	mimeType  string
	err       error
	expiresAt time.Time
}

type cacheFill struct {
	key      [sha256.Size]byte
	request  Request
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	outcome  cacheOutcome
}

type cacheOutcome struct {
	data     []byte
	mimeType string
	err      error
}

// NewCache creates a cache-first Renderer.
func NewCache(inner Renderer, config CacheConfig) (*Cache, error) {
	if inner == nil {
		return nil, errors.New("renderer must not be nil")
	}
	if strings.TrimSpace(config.BundleID) == "" {
		return nil, errors.New("BundleID must not be empty")
	}
	if config.MaxBytes <= 0 {
		return nil, errors.New("MaxBytes must be positive")
	}
	if config.MaxEntries <= 0 {
		return nil, errors.New("MaxEntries must be positive")
	}
	if config.MaxOutputBytes <= 0 {
		return nil, errors.New("MaxOutputBytes must be positive")
	}
	if config.TTL <= 0 {
		return nil, errors.New("TTL must be positive")
	}
	if config.RejectionTTL <= 0 {
		return nil, errors.New("RejectionTTL must be positive")
	}
	if config.FillTimeout <= 0 {
		return nil, errors.New("FillTimeout must be positive")
	}
	if config.MaxWaiters <= 0 {
		return nil, errors.New("MaxWaiters must be positive")
	}
	if config.MaxConcurrentFills <= 0 {
		return nil, errors.New("MaxConcurrentFills must be positive")
	}
	if config.MaxQueuedFills < 0 {
		return nil, errors.New("MaxQueuedFills must not be negative")
	}
	maxInt := int(^uint(0) >> 1)
	if config.MaxQueuedFills > maxInt-config.MaxConcurrentFills {
		return nil, errors.New("fill capacity is too large")
	}
	if config.MissesPerSecond <= 0 || math.IsNaN(config.MissesPerSecond) || math.IsInf(config.MissesPerSecond, 0) {
		return nil, errors.New("MissesPerSecond must be finite and positive")
	}
	if config.MissBurst <= 0 {
		return nil, errors.New("MissBurst must be positive")
	}
	if config.ClientMissesPerSecond <= 0 || math.IsNaN(config.ClientMissesPerSecond) || math.IsInf(config.ClientMissesPerSecond, 0) {
		return nil, errors.New("ClientMissesPerSecond must be finite and positive")
	}
	if config.ClientMissBurst <= 0 {
		return nil, errors.New("ClientMissBurst must be positive")
	}
	if config.MaxClients <= 0 {
		return nil, errors.New("MaxClients must be positive")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}

	cache := &Cache{
		inner:                 inner,
		rendererBundleID:      config.BundleID,
		maxBytes:              config.MaxBytes,
		maxEntries:            config.MaxEntries,
		maxOutputBytes:        config.MaxOutputBytes,
		ttl:                   config.TTL,
		rejectionTTL:          config.RejectionTTL,
		fillTimeout:           config.FillTimeout,
		maxPendingFills:       config.MaxConcurrentFills + config.MaxQueuedFills,
		missesPerSecond:       config.MissesPerSecond,
		missBurst:             float64(config.MissBurst),
		clientMissesPerSecond: config.ClientMissesPerSecond,
		clientMissBurst:       float64(config.ClientMissBurst),
		maxClients:            config.MaxClients,
		missAdmitter:          config.MissAdmitter,
		now:                   config.Clock,
		entries:               make(map[[sha256.Size]byte]*list.Element),
		inflight:              make(map[[sha256.Size]byte]*cacheFill),
		clientBuckets:         make(map[[sha256.Size]byte]*list.Element),
		tokens:                float64(config.MissBurst),
		lastRefill:            config.Clock(),
		fillSlots:             make(chan struct{}, config.MaxConcurrentFills),
		waiterSlots:           make(chan struct{}, config.MaxWaiters),
	}
	return cache, nil
}

// ContentKey returns the versioned SHA-256 key for a renderer bundle and request.
// Callers must validate the bundle ID and request before using the key externally.
func ContentKey(bundleID string, request Request) string {
	resolved, err := resolveCacheRequest(request)
	if err == nil {
		request = resolved
	}
	key := contentKeyBytes(bundleID, request)
	return hex.EncodeToString(key[:])
}

func contentKeyBytes(bundleID string, request Request) [sha256.Size]byte {
	hash := sha256.New()
	writeCacheKeyPart(hash, cacheKeyDomain)
	writeCacheKeyPart(hash, bundleID)
	writeCacheKeyPart(hash, string(request.Format))
	writeCacheKeyPart(hash, request.Diagram)
	var key [sha256.Size]byte
	copy(key[:], hash.Sum(nil))
	return key
}

func writeCacheKeyPart(writer io.Writer, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = io.WriteString(writer, value)
}

func resolveCacheRequest(request Request) (Request, error) {
	format, err := ParseFormat(string(request.Format))
	if err != nil {
		return Request{}, err
	}
	request.Format = format
	return request, nil
}

// Render serves a cache hit or coordinates one bounded render fill.
func (c *Cache) Render(ctx context.Context, request Request) (Result, error) {
	resolved, err := resolveCacheRequest(request)
	if err != nil {
		return Result{}, err
	}
	key := contentKeyBytes(c.rendererBundleID, resolved)
	now := c.now()

	c.mu.Lock()
	if entry, ok := c.lookupLocked(key, now); ok {
		c.counters.hits.Add(1)
		if entry.err != nil {
			c.mu.Unlock()
			return Result{}, entry.err
		}
		result := resultFromBytes(entry.data, entry.mimeType)
		c.mu.Unlock()
		return result, nil
	}
	c.counters.misses.Add(1)
	if ctx.Err() != nil {
		c.mu.Unlock()
		return Result{}, contextError(ctx.Err())
	}
	select {
	case c.waiterSlots <- struct{}{}:
	case <-ctx.Done():
		c.mu.Unlock()
		return Result{}, contextError(ctx.Err())
	default:
		c.recordOverloadLocked(&c.counters.capacityOverloads)
		c.mu.Unlock()
		return Result{}, overloadedError("rendering waiter capacity is busy")
	}
	c.mu.Unlock()
	defer func() { <-c.waiterSlots }()
	c.mu.Lock()
	if entry, ok := c.lookupLocked(key, c.now()); ok {
		c.counters.hits.Add(1)
		if entry.err != nil {
			c.mu.Unlock()
			return Result{}, entry.err
		}
		result := resultFromBytes(entry.data, entry.mimeType)
		c.mu.Unlock()
		return result, nil
	}
	if fill, ok := c.inflight[key]; ok {
		fill.waiters++
		c.counters.coalesced.Add(1)
		c.mu.Unlock()
		return c.waitForFill(ctx, fill)
	}
	if c.pendingFills >= c.maxPendingFills {
		c.recordOverloadLocked(&c.counters.capacityOverloads)
		c.mu.Unlock()
		return Result{}, overloadedError("rendering capacity is busy")
	}
	now = c.now()
	if !c.allowClientMissLocked(now, clientIdentity(ctx)) {
		c.recordOverloadLocked(&c.counters.rateOverloads)
		c.mu.Unlock()
		return Result{}, overloadedError("client rendering rate limit exceeded")
	}
	if !c.allowMissLocked(now) {
		c.recordOverloadLocked(&c.counters.rateOverloads)
		c.mu.Unlock()
		return Result{}, overloadedError("rendering rate limit exceeded")
	}

	fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fillTimeout)
	fill := &cacheFill{
		key:     key,
		request: resolved,
		ctx:     fillCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		waiters: 1,
	}
	c.inflight[key] = fill
	c.pendingFills++
	c.counters.pendingFills.Add(1)
	c.mu.Unlock()

	go c.runFill(fill)
	return c.waitForFill(ctx, fill)
}

func (c *Cache) lookupLocked(key [sha256.Size]byte, now time.Time) (*cacheEntry, bool) {
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*cacheEntry)
	if !now.Before(entry.expiresAt) {
		c.removeEntryLocked(element)
		c.counters.expirations.Add(1)
		return nil, false
	}
	c.lru.MoveToFront(element)
	return entry, true
}

func (c *Cache) allowClientMissLocked(now time.Time, key [sha256.Size]byte) bool {
	element, ok := c.clientBuckets[key]
	if !ok {
		if len(c.clientBuckets) >= c.maxClients {
			oldest := c.clientLRU.Back()
			if oldest != nil {
				delete(c.clientBuckets, oldest.Value.(*clientBucket).key)
				c.clientLRU.Remove(oldest)
			}
		}
		bucket := &clientBucket{key: key, tokens: c.clientMissBurst, lastRefill: now}
		element = c.clientLRU.PushFront(bucket)
		c.clientBuckets[key] = element
	} else {
		c.clientLRU.MoveToFront(element)
	}
	bucket := element.Value.(*clientBucket)
	if now.After(bucket.lastRefill) {
		elapsed := now.Sub(bucket.lastRefill).Seconds()
		bucket.tokens = min(c.clientMissBurst, bucket.tokens+elapsed*c.clientMissesPerSecond)
		bucket.lastRefill = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

func (c *Cache) allowMissLocked(now time.Time) bool {
	if now.After(c.lastRefill) {
		elapsed := now.Sub(c.lastRefill).Seconds()
		c.tokens = min(c.missBurst, c.tokens+elapsed*c.missesPerSecond)
		c.lastRefill = now
	}
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *Cache) recordOverloadLocked(reason *atomic.Uint64) {
	c.counters.overloads.Add(1)
	reason.Add(1)
}

func overloadedError(message string) error {
	return &Error{Code: CodeOverloaded, Message: message}
}

func (c *Cache) waitForFill(ctx context.Context, fill *cacheFill) (Result, error) {
	select {
	case <-fill.done:
		return resultFromOutcome(fill.outcome)
	default:
	}

	select {
	case <-fill.done:
		return resultFromOutcome(fill.outcome)
	case <-ctx.Done():
		c.mu.Lock()
		if fill.finished {
			outcome := fill.outcome
			c.mu.Unlock()
			return resultFromOutcome(outcome)
		}
		fill.waiters--
		cancelFill := fill.waiters == 0
		if cancelFill {
			if current, ok := c.inflight[fill.key]; ok && current == fill {
				delete(c.inflight, fill.key)
			}
		}
		c.mu.Unlock()
		if cancelFill {
			fill.cancel()
		}
		return Result{}, contextError(ctx.Err())
	}
}

func (c *Cache) runFill(fill *cacheFill) {
	if c.missAdmitter != nil {
		allowed, err := c.missAdmitter.Admit(fill.ctx)
		if fill.ctx.Err() != nil {
			c.finishFill(fill, cacheOutcome{err: contextError(fill.ctx.Err())})
			return
		}
		if err != nil || !allowed {
			c.counters.overloads.Add(1)
			c.counters.clusterOverloads.Add(1)
			c.finishFill(fill, cacheOutcome{err: overloadedError("cluster rendering budget is unavailable")})
			return
		}
	}
	select {
	case c.fillSlots <- struct{}{}:
	case <-fill.ctx.Done():
		c.finishFill(fill, cacheOutcome{err: contextError(fill.ctx.Err())})
		return
	}

	if err := fill.ctx.Err(); err != nil {
		<-c.fillSlots
		c.finishFill(fill, cacheOutcome{err: contextError(err)})
		return
	}
	c.counters.activeFills.Add(1)
	c.counters.fills.Add(1)
	outcome := c.renderFill(fill)
	c.counters.activeFills.Add(-1)
	<-c.fillSlots
	c.finishFill(fill, outcome)
}

func (c *Cache) renderFill(fill *cacheFill) cacheOutcome {
	result, err := c.inner.Render(fill.ctx, fill.request)
	if err != nil {
		if result.ReadCloser != nil {
			err = errors.Join(err, result.Close())
		}
		return cacheOutcome{err: err}
	}
	if result.ReadCloser == nil {
		return cacheOutcome{err: &Error{
			Code:    CodeInternal,
			Message: "renderer produced no image",
		}}
	}

	data, readErr := io.ReadAll(io.LimitReader(result, c.maxOutputBytes+1))
	closeErr := result.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return cacheOutcome{err: &Error{
			Code:    CodeInternal,
			Message: "could not read rendered image",
			Cause:   err,
		}}
	}
	data, err = sanitizeOutput(fill.request.Format, data)
	if err != nil {
		return cacheOutcome{err: err}
	}
	if err := validateOutput(fill.request.Format, data, c.maxOutputBytes); err != nil {
		return cacheOutcome{err: err}
	}
	return cacheOutcome{
		data:     data,
		mimeType: fill.request.Format.MIMEType(),
	}
}

func (c *Cache) finishFill(fill *cacheFill, outcome cacheOutcome) {
	c.mu.Lock()
	c.pendingFills--
	c.counters.pendingFills.Add(-1)
	if current, ok := c.inflight[fill.key]; ok && current == fill {
		delete(c.inflight, fill.key)
	}
	if err := fill.ctx.Err(); err != nil {
		outcome = cacheOutcome{err: contextError(err)}
	} else if outcome.err == nil {
		c.insertLocked(fill.key, outcome, c.now(), c.ttl)
	} else if cacheableRejection(outcome.err) {
		c.insertLocked(fill.key, outcome, c.now(), c.rejectionTTL)
	}
	fill.outcome = outcome
	fill.finished = true
	close(fill.done)
	c.mu.Unlock()
	fill.cancel()
}

func cacheableRejection(err error) bool {
	var renderErr *Error
	return errors.As(err, &renderErr) && renderErr.Code == CodeRenderRejected
}

func (c *Cache) insertLocked(key [sha256.Size]byte, outcome cacheOutcome, now time.Time, ttl time.Duration) {
	size := int64(len(outcome.data))
	if size > c.maxBytes {
		return
	}
	if existing, ok := c.entries[key]; ok {
		c.removeEntryLocked(existing)
	}
	entry := &cacheEntry{
		key:       key,
		data:      outcome.data,
		mimeType:  outcome.mimeType,
		err:       outcome.err,
		expiresAt: now.Add(ttl),
	}
	c.entries[key] = c.lru.PushFront(entry)
	c.counters.bytes.Add(size)
	c.counters.entries.Add(1)

	for c.counters.bytes.Load() > c.maxBytes || c.counters.entries.Load() > int64(c.maxEntries) {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.removeEntryLocked(oldest)
		c.counters.evictions.Add(1)
	}
}

func (c *Cache) removeEntryLocked(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(element)
	c.counters.bytes.Add(-int64(len(entry.data)))
	c.counters.entries.Add(-1)
}

func resultFromOutcome(outcome cacheOutcome) (Result, error) {
	if outcome.err != nil {
		return Result{}, outcome.err
	}
	return resultFromBytes(outcome.data, outcome.mimeType), nil
}

func resultFromBytes(data []byte, mimeType string) Result {
	return Result{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		MIMEType:   mimeType,
		Size:       int64(len(data)),
	}
}

// Stats returns an atomic snapshot. Fields can change independently during the call.
func (c *Cache) Stats() CacheStats {
	return CacheStats{
		Hits:              c.counters.hits.Load(),
		Misses:            c.counters.misses.Load(),
		Coalesced:         c.counters.coalesced.Load(),
		Overloads:         c.counters.overloads.Load(),
		CapacityOverloads: c.counters.capacityOverloads.Load(),
		RateOverloads:     c.counters.rateOverloads.Load(),
		ClusterOverloads:  c.counters.clusterOverloads.Load(),
		Evictions:         c.counters.evictions.Load(),
		Expirations:       c.counters.expirations.Load(),
		Fills:             c.counters.fills.Load(),
		Bytes:             c.counters.bytes.Load(),
		Entries:           c.counters.entries.Load(),
		PendingFills:      c.counters.pendingFills.Load(),
		ActiveFills:       c.counters.activeFills.Load(),
	}
}

var _ Renderer = (*Cache)(nil)
