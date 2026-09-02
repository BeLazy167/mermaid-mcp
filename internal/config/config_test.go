package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	clearConfigEnvironment(t)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Addr != ":8080" {
		t.Fatalf("Addr = %q, want :8080", got.Addr)
	}
	if got.MMDCPath != "mmdc" {
		t.Fatalf("MMDCPath = %q, want mmdc", got.MMDCPath)
	}
	if got.RenderConcurrency != 2 || got.MaxInFlight != 16 {
		t.Fatalf("concurrency config = %d/%d, want 2/16", got.RenderConcurrency, got.MaxInFlight)
	}
	if got.RenderTimeout != 20*time.Second {
		t.Fatalf("RenderTimeout = %v, want 20s", got.RenderTimeout)
	}
	if got.MaxDiagramBytes != 50_000 || got.MaxOutputBytes != 10*1_024*1_024 {
		t.Fatalf("size limits = %d/%d", got.MaxDiagramBytes, got.MaxOutputBytes)
	}
}

func TestLoadCloudRunPortAndOverrides(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("PORT", "9090")
	t.Setenv("RENDER_CONCURRENCY", "3")
	t.Setenv("MAX_IN_FLIGHT", "12")
	t.Setenv("MAX_DIAGRAM_BYTES", "2048")
	t.Setenv("MAX_OUTPUT_BYTES", "4096")
	t.Setenv("RENDER_TIMEOUT", "3s")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Addr != ":9090" || got.RenderConcurrency != 3 || got.MaxInFlight != 12 {
		t.Fatalf("Load() = %#v", got)
	}
	if got.MaxDiagramBytes != 2048 || got.MaxOutputBytes != 4096 || got.RenderTimeout != 3*time.Second {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero concurrency", key: "RENDER_CONCURRENCY", value: "0"},
		{name: "invalid duration", key: "RENDER_TIMEOUT", value: "soon"},
		{name: "in-flight below concurrency", key: "MAX_IN_FLIGHT", value: "1"},
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
		"ADDR", "PORT", "MMDC_PATH", "MMDC_PUPPETEER_CONFIG", "MMDC_MERMAID_CONFIG",
		"MAX_DIAGRAM_BYTES", "MAX_OUTPUT_BYTES", "RENDER_CONCURRENCY", "MAX_IN_FLIGHT", "RENDER_TIMEOUT",
	} {
		t.Setenv(key, "")
	}
}
