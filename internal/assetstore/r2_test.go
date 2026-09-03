package assetstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, time.September, 3, 3, 4, 5, 0, time.UTC)

func testKey(digit, extension string) string {
	return "v1/" + strings.Repeat(digit, 64) + extension
}

func validConfig(endpoint string) Config {
	return Config{
		Endpoint:        endpoint,
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test/secret+=key",
		Bucket:          "test-bucket",
		PublicBaseURL:   "https://assets.example.com/base/",
		ExistenceTTL:    time.Minute,
		MaxMemoEntries:  2,
		MaxObjectBytes:  2 * 1_024 * 1_024,
	}
}

func newTestR2(t *testing.T, handler http.HandlerFunc) (*R2, *httptest.Server, Config) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	config := validConfig(server.URL + "/api/")
	store, err := NewR2(config)
	if err != nil {
		t.Fatalf("NewR2() error = %v", err)
	}
	store.now = func() time.Time { return fixedTime }
	return store, server, config
}

func TestNewR2Validation(t *testing.T) {
	valid := validConfig("https://account.r2.cloudflarestorage.com")
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{name: "missing endpoint", change: func(c *Config) { c.Endpoint = "" }},
		{name: "relative endpoint", change: func(c *Config) { c.Endpoint = "/r2" }},
		{name: "endpoint scheme", change: func(c *Config) { c.Endpoint = "ftp://example.com" }},
		{name: "non-loopback HTTP endpoint", change: func(c *Config) { c.Endpoint = "http://example.com" }},
		{name: "endpoint credentials", change: func(c *Config) { c.Endpoint = "https://user:pass@example.com" }},
		{name: "endpoint query", change: func(c *Config) { c.Endpoint = "https://example.com?x=1" }},
		{name: "endpoint fragment", change: func(c *Config) { c.Endpoint = "https://example.com/#x" }},
		{name: "endpoint traversal", change: func(c *Config) { c.Endpoint = "https://example.com/a/../b" }},
		{name: "missing access key", change: func(c *Config) { c.AccessKeyID = "" }},
		{name: "unsafe access key", change: func(c *Config) { c.AccessKeyID = "key\nvalue" }},
		{name: "missing secret", change: func(c *Config) { c.SecretAccessKey = "" }},
		{name: "short bucket", change: func(c *Config) { c.Bucket = "ab" }},
		{name: "uppercase bucket", change: func(c *Config) { c.Bucket = "Bad-bucket" }},
		{name: "bucket edge hyphen", change: func(c *Config) { c.Bucket = "-bad" }},
		{name: "missing public URL", change: func(c *Config) { c.PublicBaseURL = "" }},
		{name: "relative public URL", change: func(c *Config) { c.PublicBaseURL = "/assets" }},
		{name: "HTTP public URL", change: func(c *Config) { c.PublicBaseURL = "http://127.0.0.1/assets" }},
		{name: "public URL credentials", change: func(c *Config) { c.PublicBaseURL = "https://user@example.com" }},
		{name: "public URL query", change: func(c *Config) { c.PublicBaseURL = "https://example.com?x=1" }},
		{name: "public URL traversal", change: func(c *Config) { c.PublicBaseURL = "https://example.com/a/../b" }},
		{name: "non-positive TTL", change: func(c *Config) { c.ExistenceTTL = 0 }},
		{name: "non-positive memo bound", change: func(c *Config) { c.MaxMemoEntries = 0 }},
		{name: "non-positive object bound", change: func(c *Config) { c.MaxObjectBytes = 0 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.change(&config)
			if _, err := NewR2(config); err == nil {
				t.Fatal("NewR2() error = nil")
			}
		})
	}

	store, err := NewR2(valid)
	if err != nil {
		t.Fatalf("NewR2(valid) error = %v", err)
	}
	if store == nil {
		t.Fatal("NewR2(valid) returned nil")
	}
}

func TestLookupReturnsMetadataAndMemoizes(t *testing.T) {
	var requests atomic.Int64
	data := []byte("stored svg")
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		writeHEADMetadata(w, "image/svg+xml", data)
	})
	key := testKey("a", ".svg")
	for range 2 {
		metadata, found, err := store.Lookup(context.Background(), key)
		if err != nil || !found {
			t.Fatalf("Lookup() found=%v error=%v", found, err)
		}
		wantDigest := sha256.Sum256(data)
		if metadata.URL != "https://assets.example.com/base/"+key || metadata.MIMEType != "image/svg+xml" ||
			metadata.Size != int64(len(data)) || metadata.SHA256 != hex.EncodeToString(wantDigest[:]) {
			t.Fatalf("Lookup() metadata = %#v", metadata)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestLookupMiss(t *testing.T) {
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	metadata, found, err := store.Lookup(context.Background(), testKey("a", ".png"))
	if err != nil || found || metadata != (Metadata{}) {
		t.Fatalf("Lookup() metadata=%#v found=%v error=%v", metadata, found, err)
	}
}

func TestPublishSkipsHEADAndCoalesces(t *testing.T) {
	var requests atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusCreated)
	})
	key := testKey("b", ".png")
	data := []byte("png bytes")
	results := make(chan Metadata, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			metadata, err := store.Publish(context.Background(), key, "image/png", data)
			results <- metadata
			errors <- err
		}()
	}
	<-started
	close(release)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		metadata := <-results
		if metadata.Size != int64(len(data)) || metadata.MIMEType != "image/png" {
			t.Fatalf("Publish() metadata = %#v", metadata)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestPublishRejectsMismatchAndSizeBeforeRequest(t *testing.T) {
	var requests atomic.Int64
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	store.maxObjectBytes = 3
	if _, err := store.Publish(context.Background(), testKey("a", ".png"), "image/svg+xml", []byte("x")); err == nil {
		t.Fatal("Publish() MIME mismatch error = nil")
	}
	if _, err := store.Publish(context.Background(), testKey("a", ".png"), "image/png", []byte("long")); err == nil {
		t.Fatal("Publish() size error = nil")
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d, want 0", requests.Load())
	}
}

func TestLastCanceledLookupCancelsBackgroundWork(t *testing.T) {
	var requests atomic.Int64
	started := make(chan struct{})
	canceled := make(chan struct{})
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-request.Context().Done()
			close(canceled)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	store.operationSlots = make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := store.Lookup(ctx, testKey("first", ".png"))
		firstDone <- err
	}()
	<-started
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Lookup() error = %v", err)
	}
	<-canceled
	select {
	case store.operationSlots <- struct{}{}:
		<-store.operationSlots
	case <-time.After(time.Second):
		t.Fatal("canceled operation did not release its capacity slot")
	}
	_, found, err := store.Lookup(context.Background(), testKey("second", ".png"))
	if err != nil || found {
		t.Fatalf("second Lookup() found=%v error=%v", found, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestPublishWaitsForConcurrentR2Create(t *testing.T) {
	var requests atomic.Int64
	data := []byte("winner")
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if n < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeHEADMetadata(w, "image/png", data)
	})
	metadata, err := store.Publish(context.Background(), testKey("a", ".png"), "image/png", []byte("ours"))
	if err != nil || metadata.Size != int64(len(data)) {
		t.Fatalf("Publish() metadata=%#v error=%v", metadata, err)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests = %d, want PUT and two HEADs", requests.Load())
	}
}

func TestEnsureRejectsUnsafeKeysWithoutRequest(t *testing.T) {
	var requests atomic.Int64
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	invalidUTF8 := "v1/" + string([]byte{'a', 0xff, 'b'}) + ".png"
	tooLong := strings.Repeat("a", 1025)
	invalidKeys := []string{
		"", "/image.png", "image.png/", "a//image.png", ".", "..",
		"a/./image.png", "a/../image.png", `a\image.png`, "a image.png",
		"a+image.png", "a?image.png", "a#image.png", "a~image.png", "café.png",
		"a\x00image.png", invalidUTF8, tooLong,
	}
	for _, key := range invalidKeys {
		if _, err := store.Ensure(context.Background(), key, "image/png", []byte("image")); err == nil {
			t.Errorf("Ensure(%q) error = nil", key)
		}
	}
	validKey := "assets/v1/2026-09-03/ABC_def-012.png"
	for _, mimeType := range []string{"", "image/jpeg", "image/png; charset=binary", "IMAGE/PNG"} {
		if _, err := store.Ensure(context.Background(), validKey, mimeType, []byte("image")); err == nil {
			t.Errorf("Ensure(%q, %q) error = nil", validKey, mimeType)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestEnsureMemoHitSkipsStorage(t *testing.T) {
	var requests atomic.Int64
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		writeHEADMetadata(w, "image/png", []byte("stored png"))
	})

	key := testKey("a", ".png")
	wantURL := "https://assets.example.com/base/" + key
	for i := 0; i < 2; i++ {
		got, err := store.Ensure(context.Background(), key, "image/png", nil)
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if got != wantURL {
			t.Fatalf("Ensure() URL = %q, want %q", got, wantURL)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestEnsureHEADHitSignsRequest(t *testing.T) {
	var requestErr error
	key := testKey("a", ".png")
	signatureConfig := validConfig("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			requestErr = fmt.Errorf("method = %s, want HEAD", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wantPath := "/api%20path/test-bucket/" + key
		if got := r.URL.EscapedPath(); got != wantPath {
			requestErr = fmt.Errorf("escaped path = %q, want %q", got, wantPath)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := verifySignature(r, signatureConfig); err != nil {
			requestErr = err
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got, want := signedHeaders(r), "host;x-amz-content-sha256;x-amz-date"; got != want {
			requestErr = fmt.Errorf("signed headers = %q, want %q", got, want)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeHEADMetadata(w, "image/png", []byte("stored png"))
	}))
	t.Cleanup(server.Close)
	config := validConfig(server.URL + "/api path/")
	config.PublicBaseURL = "https://assets.example.com/base path/"
	store, err := NewR2(config)
	if err != nil {
		t.Fatalf("NewR2() error = %v", err)
	}
	store.now = func() time.Time { return fixedTime }

	got, err := store.Ensure(context.Background(), key, "image/png", []byte("ignored"))
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if want := "https://assets.example.com/base%20path/" + key; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestEnsureHEADMissPUTsExactObject(t *testing.T) {
	data := []byte{0, 1, 2, 3, 255}
	key := testKey("b", ".svg")
	var methods []string
	var mu sync.Mutex
	var requestErr error
	config := validConfig("")
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if err := verifySignature(r, config); err != nil {
			requestErr = err
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				requestErr = fmt.Errorf("read PUT body: %w", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !bytes.Equal(body, data) {
				requestErr = fmt.Errorf("PUT body = %v, want %v", body, data)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			payloadSum := sha256.Sum256(data)
			for name, want := range map[string]string{
				"Content-Type":        "image/svg+xml",
				"Cache-Control":       "public,max-age=300,s-maxage=3600,immutable",
				"Content-Disposition": `inline; filename="diagram.svg"`,
				"If-None-Match":       "*",
				"X-Amz-Meta-Sha256":   hex.EncodeToString(payloadSum[:]),
			} {
				if got := r.Header.Get(name); got != want {
					requestErr = fmt.Errorf("%s = %q, want %q", name, got, want)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			wantSigned := "cache-control;content-disposition;content-type;host;if-none-match;x-amz-content-sha256;x-amz-date;x-amz-meta-sha256"
			if got := signedHeaders(r); got != wantSigned {
				requestErr = fmt.Errorf("signed headers = %q, want %q", got, wantSigned)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	got, err := store.Ensure(context.Background(), key, "image/svg+xml", data)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if want := "https://assets.example.com/base/" + key; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(methods, ","), "HEAD,PUT"; got != want {
		t.Fatalf("methods = %q, want %q", got, want)
	}
}

func TestEnsureAcceptsCreateRace(t *testing.T) {
	var requests atomic.Int64
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		if r.Method == http.MethodHead {
			if requestNumber == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeHEADMetadata(w, "image/png", []byte("winner"))
			return
		}
		w.WriteHeader(http.StatusPreconditionFailed)
	})

	for i := 0; i < 2; i++ {
		if _, err := store.Ensure(context.Background(), testKey("b", ".png"), "image/png", []byte("png")); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want HEAD, PUT, and winner HEAD", got)
	}
}

func TestEnsureStatusErrorsAreConcise(t *testing.T) {
	tests := []struct {
		name       string
		headStatus int
		putStatus  int
		want       string
	}{
		{name: "HEAD", headStatus: http.StatusServiceUnavailable, want: "R2 HEAD returned status 503"},
		{name: "PUT", headStatus: http.StatusNotFound, putStatus: http.StatusBadGateway, want: "R2 PUT returned status 502"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig("")
			store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(test.headStatus)
				} else {
					w.WriteHeader(test.putStatus)
				}
				_, _ = io.WriteString(w, config.AccessKeyID+config.SecretAccessKey+" sensitive body")
			})
			_, err := store.Ensure(context.Background(), testKey("c", ".png"), "image/png", []byte("png"))
			if err == nil {
				t.Fatal("Ensure() error = nil")
			}
			if got := err.Error(); got != test.want {
				t.Fatalf("error = %q, want %q", got, test.want)
			}
			for _, secret := range []string{config.AccessKeyID, config.SecretAccessKey, "sensitive body"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks %q: %v", secret, err)
				}
			}
		})
	}
}

func TestEnsureCancellationDoesNotCancelSharedRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	requestCanceled := make(chan struct{}, 1)
	var once sync.Once
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			writeHEADMetadata(w, "image/png", []byte("stored png"))
		case <-r.Context().Done():
			requestCanceled <- struct{}{}
		}
	})
	key := testKey("d", ".png")
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := store.Ensure(leaderCtx, key, "image/png", []byte("png"))
		leaderDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}

	followerDone := make(chan error, 1)
	go func() {
		_, err := store.Ensure(context.Background(), key, "image/png", []byte("png"))
		followerDone <- err
	}()
	cancelLeader()
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not return")
	}
	select {
	case <-requestCanceled:
		t.Fatal("leader cancellation canceled shared request")
	default:
	}
	close(release)
	select {
	case err := <-followerDone:
		if err != nil {
			t.Fatalf("follower error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower did not receive shared result")
	}
}

func TestEnsureBackgroundRequestTimeout(t *testing.T) {
	store, _, _ := newTestR2(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	store.requestTimeout = 25 * time.Millisecond
	_, err := store.Ensure(context.Background(), testKey("e", ".png"), "image/png", []byte("png"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestEnsureCoalescesSameKey(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var headRequests atomic.Int64
	var putRequests atomic.Int64
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headRequests.Add(1)
			once.Do(func() { close(started) })
			<-release
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			putRequests.Add(1)
			w.WriteHeader(http.StatusCreated)
		}
	})

	const callers = 24
	begin := make(chan struct{})
	errorsByCaller := make(chan error, callers)
	key := testKey("f", ".png")
	for range callers {
		go func() {
			<-begin
			_, err := store.Ensure(context.Background(), key, "image/png", []byte("png"))
			errorsByCaller <- err
		}()
	}
	close(begin)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	close(release)
	for range callers {
		if err := <-errorsByCaller; err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	}
	if got := headRequests.Load(); got != 1 {
		t.Fatalf("HEAD requests = %d, want 1", got)
	}
	if got := putRequests.Load(); got != 1 {
		t.Fatalf("PUT requests = %d, want 1", got)
	}
}

func TestEnsureDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err := store.Ensure(context.Background(), testKey("0", ".png"), "image/png", []byte("png"))
	if err == nil || err.Error() != "R2 HEAD returned status 307" {
		t.Fatalf("error = %v, want redirect status error", err)
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestEnsureMemoEvictionAndExpiry(t *testing.T) {
	var mu sync.Mutex
	counts := make(map[string]int)
	store, _, _ := newTestR2(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		writeHEADMetadata(w, "image/png", []byte("stored png"))
	})
	now := fixedTime
	store.now = func() time.Time { return now }
	ensure := func(key string) {
		t.Helper()
		if _, err := store.Ensure(context.Background(), key, "image/png", nil); err != nil {
			t.Fatalf("Ensure(%q) error = %v", key, err)
		}
	}

	ensure(testKey("1", ".png"))
	ensure(testKey("2", ".png"))
	ensure(testKey("1", ".png"))
	ensure(testKey("3", ".png"))
	ensure(testKey("2", ".png"))

	mu.Lock()
	if got := counts["/api/test-bucket/"+testKey("1", ".png")]; got != 1 {
		t.Errorf("one requests = %d, want 1", got)
	}
	if got := counts["/api/test-bucket/"+testKey("2", ".png")]; got != 2 {
		t.Errorf("two requests = %d, want 2 after eviction", got)
	}
	if got := counts["/api/test-bucket/"+testKey("3", ".png")]; got != 1 {
		t.Errorf("three requests = %d, want 1", got)
	}
	mu.Unlock()

	now = now.Add(time.Minute + time.Nanosecond)
	ensure(testKey("2", ".png"))
	mu.Lock()
	defer mu.Unlock()
	if got := counts["/api/test-bucket/"+testKey("2", ".png")]; got != 3 {
		t.Errorf("two requests = %d, want 3 after expiry", got)
	}
}

func writeHEADMetadata(writer http.ResponseWriter, mimeType string, data []byte) {
	digest := sha256.Sum256(data)
	writer.Header().Set("Content-Type", mimeType)
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	writer.Header().Set("X-Amz-Meta-Sha256", hex.EncodeToString(digest[:]))
	writer.WriteHeader(http.StatusOK)
}

func verifySignature(r *http.Request, config Config) error {
	authorization := r.Header.Get("Authorization")
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(authorization, prefix) {
		return fmt.Errorf("authorization scheme = %q", authorization)
	}
	parts := strings.Split(strings.TrimPrefix(authorization, prefix), ", ")
	if len(parts) != 3 {
		return fmt.Errorf("authorization parts = %d", len(parts))
	}
	credential := strings.TrimPrefix(parts[0], "Credential=")
	signed := strings.TrimPrefix(parts[1], "SignedHeaders=")
	signature := strings.TrimPrefix(parts[2], "Signature=")
	wantCredentialPrefix := config.AccessKeyID + "/"
	if !strings.HasPrefix(credential, wantCredentialPrefix) {
		return fmt.Errorf("credential = %q", credential)
	}
	scope := strings.TrimPrefix(credential, wantCredentialPrefix)
	if wantScope := fixedTime.Format("20060102") + "/auto/s3/aws4_request"; scope != wantScope {
		return fmt.Errorf("scope = %q, want %q", scope, wantScope)
	}

	var canonicalHeaders strings.Builder
	for _, name := range strings.Split(signed, ";") {
		value := r.Header.Get(name)
		if name == "host" {
			value = r.Host
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.Join(strings.Fields(value), " "))
		canonicalHeaders.WriteByte('\n')
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		r.URL.Query().Encode(),
		canonicalHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	canonicalSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		r.Header.Get("X-Amz-Date"),
		scope,
		hex.EncodeToString(canonicalSum[:]),
	}, "\n")
	dateKey := testHMAC([]byte("AWS4"+config.SecretAccessKey), fixedTime.Format("20060102"))
	regionKey := testHMAC(dateKey, "auto")
	serviceKey := testHMAC(regionKey, "s3")
	signingKey := testHMAC(serviceKey, "aws4_request")
	wantSignature := hex.EncodeToString(testHMAC(signingKey, stringToSign))
	if signature != wantSignature {
		return fmt.Errorf("signature = %q, want %q", signature, wantSignature)
	}
	return nil
}

func signedHeaders(r *http.Request) string {
	authorization := r.Header.Get("Authorization")
	for _, part := range strings.Split(authorization, ", ") {
		if strings.HasPrefix(part, "SignedHeaders=") {
			return strings.TrimPrefix(part, "SignedHeaders=")
		}
	}
	return ""
}

func testHMAC(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}
