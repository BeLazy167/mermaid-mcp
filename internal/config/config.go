// Package config loads process configuration from environment variables.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxDiagramBytes int64 = 50_000
	defaultMaxOutputBytes  int64 = 2 * 1_024 * 1_024
	defaultCacheMaxBytes   int64 = 128 * 1_024 * 1_024
)

// Config contains all runtime service settings.
type Config struct {
	Addr                    string
	NodePath                string
	WorkerScriptPath        string
	ChromiumPath            string
	RendererBundleID        string
	MaxDiagramBytes         int64
	MaxOutputBytes          int64
	RenderWorkers           int
	RenderQueueSize         int
	MaxInFlight             int
	RenderTimeout           time.Duration
	WorkerMaxRenders        int
	WorkerMaxAge            time.Duration
	WorkerMaxRSSBytes       int64
	WorkerNetworkIsolation  bool
	WorkerIsolationLauncher string
	CacheMaxBytes           int64
	CacheMaxEntries         int
	CacheTTL                time.Duration
	CacheRejectionTTL       time.Duration
	RenderMissRPS           float64
	RenderMissBurst         int
	ClientRenderMissRPS     float64
	ClientRenderMissBurst   int
	ClientLimiterEntries    int
	TrustedClientIPHeader   string
	ClusterAdmissionURL     string
	ClusterAdmissionToken   string
	R2Endpoint              string
	R2AccessKeyID           string
	R2SecretAccessKey       string
	R2Bucket                string
	AssetPublicBaseURL      string
	AssetHMACKey            []byte
	AssetExistenceTTL       time.Duration
	AssetMemoEntries        int
	ShutdownTimeout         time.Duration
}

// Load reads and validates service configuration.
func Load() (Config, error) {
	addr := envOrDefault("ADDR", "")
	if addr == "" {
		port := envOrDefault("PORT", "8080")
		parsedPort, err := strconv.Atoi(port)
		if err != nil || parsedPort < 1 || parsedPort > 65_535 {
			return Config{}, fmt.Errorf("PORT must be an integer from 1 to 65535")
		}
		addr = ":" + port
	}

	maxDiagramBytes, err := positiveInt64("MAX_DIAGRAM_BYTES", defaultMaxDiagramBytes)
	if err != nil {
		return Config{}, err
	}
	maxOutputBytes, err := positiveInt64("MAX_OUTPUT_BYTES", defaultMaxOutputBytes)
	if err != nil {
		return Config{}, err
	}
	renderWorkers, err := positiveInt("RENDER_WORKERS", 2)
	if err != nil {
		return Config{}, err
	}
	renderQueueSize, err := positiveInt("RENDER_QUEUE_SIZE", 4)
	if err != nil {
		return Config{}, err
	}
	maxInFlight, err := positiveInt("MAX_IN_FLIGHT", 64)
	if err != nil {
		return Config{}, err
	}
	if maxInFlight < renderWorkers+renderQueueSize {
		return Config{}, fmt.Errorf("MAX_IN_FLIGHT must be at least RENDER_WORKERS + RENDER_QUEUE_SIZE")
	}
	renderTimeout, err := positiveDuration("RENDER_TIMEOUT", 20*time.Second)
	if err != nil {
		return Config{}, err
	}
	workerMaxRenders, err := positiveInt("WORKER_MAX_RENDERS", 1_000)
	if err != nil {
		return Config{}, err
	}
	workerMaxAge, err := positiveDuration("WORKER_MAX_AGE", 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	workerMaxRSSBytes, err := positiveInt64("WORKER_MAX_RSS_BYTES", 1_024*1_024*1_024)
	if err != nil {
		return Config{}, err
	}
	workerNetworkIsolation, err := boolean("WORKER_NETWORK_ISOLATION", true)
	if err != nil {
		return Config{}, err
	}
	cacheMaxBytes, err := positiveInt64("CACHE_MAX_BYTES", defaultCacheMaxBytes)
	if err != nil {
		return Config{}, err
	}
	cacheMaxEntries, err := positiveInt("CACHE_MAX_ENTRIES", 10_000)
	if err != nil {
		return Config{}, err
	}
	cacheTTL, err := positiveDuration("CACHE_TTL", 10*time.Minute)
	if err != nil {
		return Config{}, err
	}
	cacheRejectionTTL, err := positiveDuration("CACHE_REJECTION_TTL", 20*time.Second)
	if err != nil {
		return Config{}, err
	}
	renderMissRPS, err := positiveFloat("RENDER_MISS_RPS", 5)
	if err != nil {
		return Config{}, err
	}
	renderMissBurst, err := positiveInt("RENDER_MISS_BURST", 6)
	if err != nil {
		return Config{}, err
	}
	clientRenderMissRPS, err := positiveFloat("CLIENT_RENDER_MISS_RPS", 1)
	if err != nil {
		return Config{}, err
	}
	clientRenderMissBurst, err := positiveInt("CLIENT_RENDER_MISS_BURST", 3)
	if err != nil {
		return Config{}, err
	}
	clientLimiterEntries, err := positiveInt("CLIENT_LIMITER_ENTRIES", 100_000)
	if err != nil {
		return Config{}, err
	}
	clusterAdmissionURL := os.Getenv("CLUSTER_ADMISSION_URL")
	clusterAdmissionToken := os.Getenv("CLUSTER_ADMISSION_TOKEN")
	if (clusterAdmissionURL == "") != (clusterAdmissionToken == "") {
		return Config{}, fmt.Errorf("CLUSTER_ADMISSION_URL and CLUSTER_ADMISSION_TOKEN must be set together")
	}
	if clusterAdmissionToken != "" && len(clusterAdmissionToken) < 32 {
		return Config{}, fmt.Errorf("CLUSTER_ADMISSION_TOKEN must contain at least 32 bytes")
	}
	assetExistenceTTL, err := positiveDuration("ASSET_EXISTENCE_TTL", time.Hour)
	if err != nil {
		return Config{}, err
	}
	assetMemoEntries, err := positiveInt("ASSET_MEMO_ENTRIES", 100_000)
	if err != nil {
		return Config{}, err
	}
	r2Endpoint := os.Getenv("R2_ENDPOINT")
	r2AccessKeyID := os.Getenv("R2_ACCESS_KEY_ID")
	r2SecretAccessKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	r2Bucket := os.Getenv("R2_BUCKET")
	assetPublicBaseURL := os.Getenv("ASSET_PUBLIC_BASE_URL")
	assetHMACKeyEncoded := os.Getenv("ASSET_HMAC_KEY")
	storageValues := []string{r2Endpoint, r2AccessKeyID, r2SecretAccessKey, r2Bucket, assetPublicBaseURL, assetHMACKeyEncoded}
	configuredValues := 0
	for _, value := range storageValues {
		if value != "" {
			configuredValues++
		}
	}
	if configuredValues != 0 && configuredValues != len(storageValues) {
		return Config{}, fmt.Errorf("R2_ENDPOINT, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET, ASSET_PUBLIC_BASE_URL, and ASSET_HMAC_KEY must be set together")
	}
	rendererBundleID := envOrDefault("RENDERER_BUNDLE_ID", "dev")
	if configuredValues == len(storageValues) && (os.Getenv("RENDERER_BUNDLE_ID") == "" || rendererBundleID == "dev" || strings.HasPrefix(rendererBundleID, "dev-")) {
		return Config{}, fmt.Errorf("RENDERER_BUNDLE_ID must be an explicit immutable build ID when URL delivery is configured")
	}
	var assetHMACKey []byte
	if assetHMACKeyEncoded != "" {
		assetHMACKey, err = base64.StdEncoding.DecodeString(assetHMACKeyEncoded)
		if err != nil || len(assetHMACKey) < 32 {
			return Config{}, fmt.Errorf("ASSET_HMAC_KEY must be base64 for at least 32 random bytes")
		}
	}

	return Config{
		Addr:                    addr,
		NodePath:                envOrDefault("NODE_PATH", "node"),
		WorkerScriptPath:        envOrDefault("RENDER_WORKER_SCRIPT", "renderer/worker.mjs"),
		ChromiumPath:            envOrDefault("CHROMIUM_PATH", ""),
		RendererBundleID:        rendererBundleID,
		MaxDiagramBytes:         maxDiagramBytes,
		MaxOutputBytes:          maxOutputBytes,
		RenderWorkers:           renderWorkers,
		RenderQueueSize:         renderQueueSize,
		MaxInFlight:             maxInFlight,
		RenderTimeout:           renderTimeout,
		WorkerMaxRenders:        workerMaxRenders,
		WorkerMaxAge:            workerMaxAge,
		WorkerMaxRSSBytes:       workerMaxRSSBytes,
		WorkerNetworkIsolation:  workerNetworkIsolation,
		WorkerIsolationLauncher: envOrDefault("WORKER_ISOLATION_LAUNCHER", "/usr/local/bin/renderer-launcher"),
		CacheMaxBytes:           cacheMaxBytes,
		CacheMaxEntries:         cacheMaxEntries,
		CacheTTL:                cacheTTL,
		CacheRejectionTTL:       cacheRejectionTTL,
		RenderMissRPS:           renderMissRPS,
		RenderMissBurst:         renderMissBurst,
		ClientRenderMissRPS:     clientRenderMissRPS,
		ClientRenderMissBurst:   clientRenderMissBurst,
		ClientLimiterEntries:    clientLimiterEntries,
		TrustedClientIPHeader:   os.Getenv("TRUSTED_CLIENT_IP_HEADER"),
		ClusterAdmissionURL:     clusterAdmissionURL,
		ClusterAdmissionToken:   clusterAdmissionToken,
		R2Endpoint:              r2Endpoint,
		R2AccessKeyID:           r2AccessKeyID,
		R2SecretAccessKey:       r2SecretAccessKey,
		R2Bucket:                r2Bucket,
		AssetPublicBaseURL:      assetPublicBaseURL,
		AssetHMACKey:            assetHMACKey,
		AssetExistenceTTL:       assetExistenceTTL,
		AssetMemoEntries:        assetMemoEntries,
		ShutdownTimeout:         10 * time.Second,
	}, nil
}

// AssetStorageConfigured reports whether optional URL delivery is enabled.
func (c Config) AssetStorageConfigured() bool {
	return c.R2Endpoint != ""
}

func boolean(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return parsed, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func positiveInt(key string, fallback int) (int, error) {
	value := envOrDefault(key, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func positiveInt64(key string, fallback int64) (int64, error) {
	value := envOrDefault(key, strconv.FormatInt(fallback, 10))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func positiveFloat(key string, fallback float64) (float64, error) {
	value := envOrDefault(key, strconv.FormatFloat(fallback, 'f', -1, 64))
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	return parsed, nil
}

func positiveDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := envOrDefault(key, fallback.String())
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return parsed, nil
}
