// Package assetstore stores immutable rendered assets.
package assetstore

import (
	"bytes"
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxObjectKeyBytes       = 1024
	maxResponseDrain        = 4 * 1024
	defaultRequestTimeout   = 30 * time.Second
	maxIdleConnections      = 32
	maxHostConnections      = 16
	maxConcurrentOperations = maxHostConnections
)

// Config configures a Cloudflare R2 asset store.
type Config struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	PublicBaseURL   string
	ExistenceTTL    time.Duration
	MaxMemoEntries  int
	MaxObjectBytes  int64
}

// Metadata describes one immutable stored asset.
type Metadata struct {
	URL      string
	MIMEType string
	SHA256   string
	Size     int64
}

// R2 stores immutable assets in a Cloudflare R2 bucket through its S3 API.
type R2 struct {
	endpoint        *url.URL
	publicBaseURL   *url.URL
	accessKeyID     string
	secretAccessKey string
	bucket          string
	existenceTTL    time.Duration
	maxMemoEntries  int
	maxObjectBytes  int64
	requestTimeout  time.Duration
	client          *http.Client
	now             func() time.Time

	memoMu sync.Mutex
	memo   map[string]*list.Element
	lru    list.List

	flightMu        sync.Mutex
	metadataFlights map[string]*metadataFlight
	operationSlots  chan struct{}
}

type memoEntry struct {
	key       string
	metadata  Metadata
	expiresAt time.Time
}

type metadataFlight struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	metadata Metadata
	found    bool
	err      error
}

// NewR2 validates config and creates an R2 asset store.
func NewR2(config Config) (*R2, error) {
	endpoint, err := parseBaseURL("Endpoint", config.Endpoint, true)
	if err != nil {
		return nil, err
	}
	publicBaseURL, err := parseBaseURL("PublicBaseURL", config.PublicBaseURL, false)
	if err != nil {
		return nil, err
	}
	if !validAccessKeyID(config.AccessKeyID) {
		return nil, errors.New("AccessKeyID is invalid")
	}
	if config.SecretAccessKey == "" {
		return nil, errors.New("SecretAccessKey is required")
	}
	if !validBucket(config.Bucket) {
		return nil, errors.New("bucket is invalid")
	}
	if config.ExistenceTTL <= 0 {
		return nil, errors.New("ExistenceTTL must be positive")
	}
	if config.MaxMemoEntries <= 0 {
		return nil, errors.New("MaxMemoEntries must be positive")
	}
	if config.MaxObjectBytes <= 0 {
		return nil, errors.New("MaxObjectBytes must be positive")
	}

	var transport *http.Transport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	} else {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxIdleConns = maxIdleConnections
	transport.MaxIdleConnsPerHost = maxHostConnections
	transport.MaxConnsPerHost = maxHostConnections
	transport.MaxResponseHeaderBytes = 1 << 20

	return &R2{
		endpoint:        endpoint,
		publicBaseURL:   publicBaseURL,
		accessKeyID:     config.AccessKeyID,
		secretAccessKey: config.SecretAccessKey,
		bucket:          config.Bucket,
		existenceTTL:    config.ExistenceTTL,
		maxMemoEntries:  config.MaxMemoEntries,
		maxObjectBytes:  config.MaxObjectBytes,
		requestTimeout:  defaultRequestTimeout,
		client: &http.Client{
			Transport: transport,
			Timeout:   defaultRequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:             time.Now,
		memo:            make(map[string]*list.Element),
		metadataFlights: make(map[string]*metadataFlight),
		operationSlots:  make(chan struct{}, maxConcurrentOperations),
	}, nil
}

// Lookup returns an existing immutable object without downloading it.
func (r *R2) Lookup(ctx context.Context, key string) (Metadata, bool, error) {
	if err := validateKey(key); err != nil {
		return Metadata{}, false, err
	}
	if metadata, ok := r.memoMetadata(key); ok {
		return metadata, true, nil
	}
	return r.coordinateMetadata(ctx, "lookup\x00"+key, func(workCtx context.Context) (Metadata, bool, error) {
		metadata, found, err := r.lookupObject(workCtx, key)
		if err == nil && found {
			r.memoizeMetadata(key, metadata)
		}
		return metadata, found, err
	})
}

// Publish creates an immutable object or returns the winner of a concurrent create.
func (r *R2) Publish(ctx context.Context, key, mimeType string, data []byte) (Metadata, error) {
	if err := validateKey(key); err != nil {
		return Metadata{}, err
	}
	if err := validateMIMEAndKey(key, mimeType); err != nil {
		return Metadata{}, err
	}
	if int64(len(data)) > r.maxObjectBytes || len(data) == 0 {
		return Metadata{}, errors.New("asset data exceeds its size limit")
	}
	if metadata, ok := r.memoMetadata(key); ok {
		return metadata, nil
	}
	requestData := bytes.Clone(data)
	metadata, _, err := r.coordinateMetadata(ctx, "publish\x00"+key, func(workCtx context.Context) (Metadata, bool, error) {
		published, publishErr := r.publishObject(workCtx, key, mimeType, requestData)
		if publishErr == nil {
			r.memoizeMetadata(key, published)
		}
		return published, true, publishErr
	})
	return metadata, err
}

func (r *R2) coordinateMetadata(
	ctx context.Context,
	flightKey string,
	work func(context.Context) (Metadata, bool, error),
) (Metadata, bool, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, false, err
	}
	r.flightMu.Lock()
	if flight, ok := r.metadataFlights[flightKey]; ok {
		flight.waiters++
		r.flightMu.Unlock()
		return r.waitForMetadataFlight(ctx, flightKey, flight)
	}
	flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	flight := &metadataFlight{ctx: flightCtx, cancel: cancel, done: make(chan struct{}), waiters: 1}
	r.metadataFlights[flightKey] = flight
	r.flightMu.Unlock()

	select {
	case r.operationSlots <- struct{}{}:
	case <-ctx.Done():
		r.finishFlight(flightKey, flight, Metadata{}, false, ctx.Err())
		return Metadata{}, false, ctx.Err()
	default:
		err := errors.New("asset store operation capacity is full")
		r.finishFlight(flightKey, flight, Metadata{}, false, err)
		return Metadata{}, false, err
	}

	go func() {
		requestCtx, cancelRequest := context.WithTimeout(flight.ctx, r.requestTimeout)
		metadata, found, err := work(requestCtx)
		cancelRequest()
		<-r.operationSlots
		r.finishFlight(flightKey, flight, metadata, found, err)
	}()
	return r.waitForMetadataFlight(ctx, flightKey, flight)
}

func (r *R2) waitForMetadataFlight(ctx context.Context, flightKey string, flight *metadataFlight) (Metadata, bool, error) {
	select {
	case <-flight.done:
		return flight.metadata, flight.found, flight.err
	case <-ctx.Done():
		r.flightMu.Lock()
		if flight.finished {
			metadata, found, err := flight.metadata, flight.found, flight.err
			r.flightMu.Unlock()
			return metadata, found, err
		}
		flight.waiters--
		if flight.waiters == 0 {
			if r.metadataFlights[flightKey] == flight {
				delete(r.metadataFlights, flightKey)
			}
			flight.cancel()
		}
		r.flightMu.Unlock()
		return Metadata{}, false, ctx.Err()
	}
}

func (r *R2) finishFlight(flightKey string, flight *metadataFlight, metadata Metadata, found bool, err error) {
	r.flightMu.Lock()
	flight.metadata, flight.found, flight.err = metadata, found, err
	flight.finished = true
	if r.metadataFlights[flightKey] == flight {
		delete(r.metadataFlights, flightKey)
	}
	close(flight.done)
	r.flightMu.Unlock()
	flight.cancel()
}

func (r *R2) lookupObject(ctx context.Context, key string) (Metadata, bool, error) {
	response, err := r.request(ctx, http.MethodHead, key, "", nil)
	if err != nil {
		return Metadata{}, false, err
	}
	switch response.status {
	case http.StatusNotFound:
		return Metadata{}, false, nil
	case http.StatusOK:
		metadata, err := r.metadataFromHEAD(key, response)
		return metadata, err == nil, err
	default:
		return Metadata{}, false, fmt.Errorf("R2 HEAD returned status %d", response.status)
	}
}

func (r *R2) publishObject(ctx context.Context, key, mimeType string, data []byte) (Metadata, error) {
	response, err := r.request(ctx, http.MethodPut, key, mimeType, data)
	if err != nil {
		return Metadata{}, err
	}
	metadata := r.metadataFromData(key, mimeType, data)
	switch response.status {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return metadata, nil
	case http.StatusPreconditionFailed:
		existing, found, lookupErr := r.lookupObject(ctx, key)
		if lookupErr != nil {
			return Metadata{}, lookupErr
		}
		if !found {
			return Metadata{}, errors.New("R2 create race produced no object")
		}
		return existing, nil
	case http.StatusTooManyRequests:
		return r.waitForExisting(ctx, key)
	default:
		return Metadata{}, fmt.Errorf("R2 PUT returned status %d", response.status)
	}
}

func (r *R2) waitForExisting(ctx context.Context, key string) (Metadata, error) {
	delay := 25 * time.Millisecond
	for range 5 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Metadata{}, ctx.Err()
		case <-timer.C:
		}
		metadata, found, err := r.lookupObject(ctx, key)
		if err != nil {
			return Metadata{}, err
		}
		if found {
			return metadata, nil
		}
		delay *= 2
	}
	return Metadata{}, errors.New("R2 concurrent create did not become visible")
}

func (r *R2) metadataFromHEAD(key string, response storageResponse) (Metadata, error) {
	mimeType, _, err := mime.ParseMediaType(response.header.Get("Content-Type"))
	if err != nil || validateMIMEAndKey(key, mimeType) != nil {
		return Metadata{}, errors.New("R2 object metadata is invalid")
	}
	if response.contentLength <= 0 || response.contentLength > r.maxObjectBytes {
		return Metadata{}, errors.New("R2 object metadata is invalid")
	}
	digest := strings.ToLower(strings.TrimSpace(response.header.Get("X-Amz-Meta-Sha256")))
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return Metadata{}, errors.New("R2 object metadata is invalid")
	}
	return Metadata{
		URL:      appendPath(r.publicBaseURL, key),
		MIMEType: mimeType,
		SHA256:   digest,
		Size:     response.contentLength,
	}, nil
}

func (r *R2) metadataFromData(key, mimeType string, data []byte) Metadata {
	digest := sha256.Sum256(data)
	return Metadata{
		URL:      appendPath(r.publicBaseURL, key),
		MIMEType: mimeType,
		SHA256:   hex.EncodeToString(digest[:]),
		Size:     int64(len(data)),
	}
}

// Ensure makes an immutable object available and returns its public URL.
func (r *R2) Ensure(ctx context.Context, key, mimeType string, data []byte) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if err := validateMIMEAndKey(key, mimeType); err != nil {
		return "", err
	}
	metadata, found, err := r.Lookup(ctx, key)
	if err != nil {
		return "", err
	}
	if found {
		return metadata.URL, nil
	}
	metadata, err = r.Publish(ctx, key, mimeType, data)
	if err != nil {
		return "", err
	}
	return metadata.URL, nil
}

type storageResponse struct {
	status        int
	header        http.Header
	contentLength int64
}

func (r *R2) request(ctx context.Context, method, key, mimeType string, data []byte) (storageResponse, error) {
	objectURL := appendPath(r.endpoint, r.bucket, key)
	var body io.Reader
	if method == http.MethodPut {
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, objectURL, body)
	if err != nil {
		return storageResponse{}, fmt.Errorf("R2 %s request is invalid", method)
	}

	payloadHash := sha256.Sum256(data)
	if method == http.MethodHead {
		payloadHash = sha256.Sum256(nil)
	} else {
		payloadHex := hex.EncodeToString(payloadHash[:])
		req.Header.Set("Content-Type", mimeType)
		req.Header.Set("Cache-Control", "public,max-age=300,s-maxage=3600,immutable")
		filename := "diagram.png"
		if mimeType == "image/svg+xml" {
			filename = "diagram.svg"
		}
		req.Header.Set("Content-Disposition", `inline; filename="`+filename+`"`)
		req.Header.Set("If-None-Match", "*")
		req.Header.Set("X-Amz-Meta-Sha256", payloadHex)
		// The asset CDN must add nosniff and a restrictive Content-Security-Policy.
	}
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(payloadHash[:]))
	r.sign(req, payloadHash)

	response, err := r.client.Do(req)
	if err != nil {
		return storageResponse{}, safeRequestError(method, ctx, err)
	}
	result := storageResponse{
		status:        response.StatusCode,
		header:        response.Header.Clone(),
		contentLength: response.ContentLength,
	}
	drainAndClose(response.Body)
	return result, nil
}

func (r *R2) sign(req *http.Request, payloadHash [sha256.Size]byte) {
	now := r.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	signedHeaderNames := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Method == http.MethodPut {
		signedHeaderNames = []string{
			"cache-control",
			"content-disposition",
			"content-type",
			"host",
			"if-none-match",
			"x-amz-content-sha256",
			"x-amz-date",
			"x-amz-meta-sha256",
		}
	}
	signedHeaders := strings.Join(signedHeaderNames, ";")
	canonicalHeaders := canonicalHeaders(req, signedHeaderNames)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.Query().Encode(),
		canonicalHeaders,
		signedHeaders,
		hex.EncodeToString(payloadHash[:]),
	}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	scope := date + "/auto/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(canonicalHash[:]),
	}, "\n")

	dateKey := hmacSHA256([]byte("AWS4"+r.secretAccessKey), date)
	regionKey := hmacSHA256(dateKey, "auto")
	serviceKey := hmacSHA256(regionKey, "s3")
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+r.accessKeyID+"/"+scope+", "+
		"SignedHeaders="+signedHeaders+", "+
		"Signature="+signature)
}

func canonicalHeaders(req *http.Request, names []string) string {
	var result strings.Builder
	for _, name := range names {
		value := req.Header.Get(name)
		if name == "host" {
			value = req.URL.Host
		}
		result.WriteString(name)
		result.WriteByte(':')
		result.WriteString(strings.Join(strings.Fields(value), " "))
		result.WriteByte('\n')
	}
	return result.String()
}

func hmacSHA256(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}

func safeRequestError(method string, ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("R2 %s request: %w", method, ctxErr)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("R2 %s request: %w", method, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("R2 %s request: %w", method, context.DeadlineExceeded)
	}
	return fmt.Errorf("R2 %s request failed", method)
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, maxResponseDrain)
	_ = body.Close()
}

func (r *R2) memoMetadata(key string) (Metadata, bool) {
	r.memoMu.Lock()
	defer r.memoMu.Unlock()

	element, ok := r.memo[key]
	if !ok {
		return Metadata{}, false
	}
	entry := element.Value.(*memoEntry)
	if !r.now().Before(entry.expiresAt) {
		delete(r.memo, key)
		r.lru.Remove(element)
		return Metadata{}, false
	}
	r.lru.MoveToFront(element)
	return entry.metadata, true
}

func (r *R2) memoizeMetadata(key string, metadata Metadata) {
	r.memoMu.Lock()
	defer r.memoMu.Unlock()

	expiresAt := r.now().Add(r.existenceTTL)
	if element, ok := r.memo[key]; ok {
		entry := element.Value.(*memoEntry)
		entry.metadata = metadata
		entry.expiresAt = expiresAt
		r.lru.MoveToFront(element)
		return
	}
	element := r.lru.PushFront(&memoEntry{key: key, metadata: metadata, expiresAt: expiresAt})
	r.memo[key] = element
	if len(r.memo) <= r.maxMemoEntries {
		return
	}
	oldest := r.lru.Back()
	entry := oldest.Value.(*memoEntry)
	delete(r.memo, entry.key)
	r.lru.Remove(oldest)
}

func parseBaseURL(name, value string, allowLoopbackHTTP bool) (*url.URL, error) {
	if value == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" {
		if scheme != "http" || !allowLoopbackHTTP || !isLoopbackHost(parsed.Hostname()) {
			return nil, fmt.Errorf("%s is invalid", name)
		}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	if err := validateBasePath(parsed.Path); err != nil {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func validateBasePath(value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\\') {
		return errors.New("unsafe path")
	}
	trimmed := strings.Trim(value, "/")
	if trimmed == "" {
		return nil
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("unsafe path")
		}
		for _, character := range segment {
			if character < 0x20 || character == 0x7f {
				return errors.New("unsafe path")
			}
		}
	}
	return nil
}

func validAccessKeyID(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validBucket(value string) bool {
	if len(value) < 3 || len(value) > 63 {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if character == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func validateKey(key string) error {
	if key == "" || len(key) > maxObjectKeyBytes {
		return errors.New("asset key is invalid")
	}
	for _, character := range key {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '/' || character == '-' {
			continue
		}
		return errors.New("asset key is invalid")
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("asset key is invalid")
		}
	}
	return nil
}

func validateMIMEType(value string) error {
	if value != "image/png" && value != "image/svg+xml" {
		return errors.New("MIME type is invalid")
	}
	return nil
}

func validateMIMEAndKey(key, value string) error {
	if err := validateMIMEType(value); err != nil {
		return err
	}
	if value == "image/png" && strings.HasSuffix(key, ".png") ||
		value == "image/svg+xml" && strings.HasSuffix(key, ".svg") {
		return nil
	}
	return errors.New("MIME type does not match asset key")
}

func appendPath(base *url.URL, parts ...string) string {
	copyURL := *base
	pathValue := strings.TrimRight(copyURL.Path, "/")
	for _, part := range parts {
		pathValue += "/" + part
	}
	if pathValue == "" || pathValue[0] != '/' {
		pathValue = "/" + pathValue
	}
	copyURL.Path = pathValue
	copyURL.RawPath = awsEscapePath(pathValue)
	return copyURL.String()
}

func awsEscapePath(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var escaped strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' || character == '~' ||
			character == '/' {
			escaped.WriteByte(character)
			continue
		}
		escaped.WriteByte('%')
		escaped.WriteByte(hexDigits[character>>4])
		escaped.WriteByte(hexDigits[character&0x0f])
	}
	return escaped.String()
}
