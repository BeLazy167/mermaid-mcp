package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	clearConfigEnvironment(t)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Addr != ":8080" || got.NodePath != "node" || got.WorkerScriptPath != "renderer/worker.mjs" {
		t.Fatalf("process config = %#v", got)
	}
	if got.RenderWorkers != 2 || got.RenderQueueSize != 4 || got.MaxInFlight != 64 {
		t.Fatalf("concurrency config = %d/%d/%d", got.RenderWorkers, got.RenderQueueSize, got.MaxInFlight)
	}
	if got.RenderTimeout != 20*time.Second || got.WorkerMaxAge != 30*time.Minute || got.WorkerMaxRenders != 1_000 || got.WorkerMaxRSSBytes != 1_024*1_024*1_024 || !got.WorkerNetworkIsolation || got.WorkerIsolationLauncher != "/usr/local/bin/renderer-launcher" {
		t.Fatalf("worker config = %#v", got)
	}
	if got.MaxDiagramBytes != 50_000 || got.MaxOutputBytes != 2*1_024*1_024 {
		t.Fatalf("size limits = %d/%d", got.MaxDiagramBytes, got.MaxOutputBytes)
	}
	if got.CacheMaxBytes != 128*1_024*1_024 || got.CacheMaxEntries != 10_000 || got.CacheTTL != 10*time.Minute || got.CacheRejectionTTL != 20*time.Second {
		t.Fatalf("cache config = %d/%d/%v", got.CacheMaxBytes, got.CacheMaxEntries, got.CacheTTL)
	}
	if got.RenderMissRPS != 5 || got.RenderMissBurst != 6 || got.ClientRenderMissRPS != 1 || got.ClientRenderMissBurst != 3 || got.ClientLimiterEntries != 100_000 {
		t.Fatalf("miss config = %v/%d", got.RenderMissRPS, got.RenderMissBurst)
	}
	if got.AssetStorageConfigured() || got.AssetExistenceTTL != time.Hour || got.AssetMemoEntries != 100_000 {
		t.Fatalf("asset config = %#v", got)
	}
}

func TestLoadCloudPortAndOverrides(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("PORT", "9090")
	t.Setenv("NODE_PATH", "/node")
	t.Setenv("RENDER_WORKER_SCRIPT", "/worker.mjs")
	t.Setenv("CHROMIUM_PATH", "/chromium")
	t.Setenv("RENDERER_BUNDLE_ID", "bundle-v1")
	t.Setenv("RENDER_WORKERS", "3")
	t.Setenv("RENDER_QUEUE_SIZE", "2")
	t.Setenv("MAX_IN_FLIGHT", "12")
	t.Setenv("MAX_DIAGRAM_BYTES", "2048")
	t.Setenv("MAX_OUTPUT_BYTES", "4096")
	t.Setenv("RENDER_TIMEOUT", "3s")
	t.Setenv("WORKER_MAX_RENDERS", "50")
	t.Setenv("WORKER_MAX_AGE", "1m")
	t.Setenv("WORKER_MAX_RSS_BYTES", "1048576")
	t.Setenv("WORKER_NETWORK_ISOLATION", "false")
	t.Setenv("CACHE_MAX_BYTES", "8192")
	t.Setenv("CACHE_MAX_ENTRIES", "12")
	t.Setenv("CACHE_TTL", "2m")
	t.Setenv("CACHE_REJECTION_TTL", "15s")
	t.Setenv("RENDER_MISS_RPS", "7.5")
	t.Setenv("RENDER_MISS_BURST", "8")
	t.Setenv("CLIENT_RENDER_MISS_RPS", "2.5")
	t.Setenv("CLIENT_RENDER_MISS_BURST", "4")
	t.Setenv("CLIENT_LIMITER_ENTRIES", "500")
	t.Setenv("CLUSTER_ADMISSION_URL", "https://admission.example.com/admit")
	t.Setenv("CLUSTER_ADMISSION_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("R2_ENDPOINT", "https://account.r2.cloudflarestorage.com")
	t.Setenv("R2_ACCESS_KEY_ID", "access")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret")
	t.Setenv("R2_BUCKET", "renders")
	t.Setenv("ASSET_PUBLIC_BASE_URL", "https://assets.example.com")
	t.Setenv("ASSET_HMAC_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	t.Setenv("ASSET_EXISTENCE_TTL", "30m")
	t.Setenv("ASSET_MEMO_ENTRIES", "900")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Addr != ":9090" || got.NodePath != "/node" || got.WorkerScriptPath != "/worker.mjs" || got.ChromiumPath != "/chromium" {
		t.Fatalf("Load() = %#v", got)
	}
	if got.RendererBundleID != "bundle-v1" || got.RenderWorkers != 3 || got.RenderQueueSize != 2 || got.MaxInFlight != 12 {
		t.Fatalf("Load() = %#v", got)
	}
	if got.MaxDiagramBytes != 2048 || got.MaxOutputBytes != 4096 || got.RenderTimeout != 3*time.Second {
		t.Fatalf("Load() = %#v", got)
	}
	if got.WorkerMaxRenders != 50 || got.WorkerMaxAge != time.Minute || got.WorkerMaxRSSBytes != 1_048_576 || got.WorkerNetworkIsolation || got.CacheMaxBytes != 8192 || got.CacheMaxEntries != 12 || got.CacheTTL != 2*time.Minute || got.CacheRejectionTTL != 15*time.Second {
		t.Fatalf("Load() = %#v", got)
	}
	if got.RenderMissRPS != 7.5 || got.RenderMissBurst != 8 || got.ClientRenderMissRPS != 2.5 || got.ClientRenderMissBurst != 4 || got.ClientLimiterEntries != 500 || got.ClusterAdmissionURL != "https://admission.example.com/admit" {
		t.Fatalf("Load() = %#v", got)
	}
	if !got.AssetStorageConfigured() || got.R2Bucket != "renders" || len(got.AssetHMACKey) != 32 || got.AssetExistenceTTL != 30*time.Minute || got.AssetMemoEntries != 900 {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestLoadRejectsInvalidClusterAdmission(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("CLUSTER_ADMISSION_URL", "https://admission.example.com/admit")
	if _, err := Load(); err == nil {
		t.Fatal("Load() missing token error = nil")
	}
	clearConfigEnvironment(t)
	t.Setenv("CLUSTER_ADMISSION_URL", "https://admission.example.com/admit")
	t.Setenv("CLUSTER_ADMISSION_TOKEN", "short")
	if _, err := Load(); err == nil {
		t.Fatal("Load() short token error = nil")
	}
}

func TestLoadRejectsPartialAssetStorage(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("R2_ENDPOINT", "https://account.r2.cloudflarestorage.com")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil")
	}
}

func TestLoadRejectsAssetStorageWithoutExplicitBundle(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("R2_ENDPOINT", "https://account.r2.cloudflarestorage.com")
	t.Setenv("R2_ACCESS_KEY_ID", "access")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret")
	t.Setenv("R2_BUCKET", "renders")
	t.Setenv("ASSET_PUBLIC_BASE_URL", "https://assets.example.com")
	t.Setenv("ASSET_HMAC_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil")
	}
}

func TestLoadRejectsDevelopmentBundleForAssetStorage(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("R2_ENDPOINT", "https://account.r2.cloudflarestorage.com")
	t.Setenv("R2_ACCESS_KEY_ID", "access")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret")
	t.Setenv("R2_BUCKET", "renders")
	t.Setenv("ASSET_PUBLIC_BASE_URL", "https://assets.example.com")
	t.Setenv("ASSET_HMAC_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	t.Setenv("RENDERER_BUNDLE_ID", "dev-mermaid")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero workers", key: "RENDER_WORKERS", value: "0"},
		{name: "zero queue", key: "RENDER_QUEUE_SIZE", value: "0"},
		{name: "invalid duration", key: "RENDER_TIMEOUT", value: "soon"},
		{name: "invalid miss rate", key: "RENDER_MISS_RPS", value: "0"},
		{name: "in-flight below worker and queue capacity", key: "MAX_IN_FLIGHT", value: "5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ADDR", "PORT", "NODE_PATH", "RENDER_WORKER_SCRIPT", "CHROMIUM_PATH", "RENDERER_BUNDLE_ID",
		"MAX_DIAGRAM_BYTES", "MAX_OUTPUT_BYTES", "RENDER_WORKERS", "RENDER_QUEUE_SIZE", "MAX_IN_FLIGHT",
		"RENDER_TIMEOUT", "WORKER_MAX_RENDERS", "WORKER_MAX_AGE", "WORKER_MAX_RSS_BYTES", "WORKER_NETWORK_ISOLATION", "WORKER_ISOLATION_LAUNCHER", "CACHE_MAX_BYTES", "CACHE_MAX_ENTRIES", "CACHE_TTL", "CACHE_REJECTION_TTL",
		"RENDER_MISS_RPS", "RENDER_MISS_BURST", "CLIENT_RENDER_MISS_RPS", "CLIENT_RENDER_MISS_BURST", "CLIENT_LIMITER_ENTRIES", "TRUSTED_CLIENT_IP_HEADER", "CLUSTER_ADMISSION_URL", "CLUSTER_ADMISSION_TOKEN", "R2_ENDPOINT", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY",
		"R2_BUCKET", "ASSET_PUBLIC_BASE_URL", "ASSET_HMAC_KEY", "ASSET_EXISTENCE_TTL", "ASSET_MEMO_ENTRIES",
	} {
		t.Setenv(key, "")
	}
}
