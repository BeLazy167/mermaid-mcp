// Package config loads process configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultMaxDiagramBytes int64 = 50_000
	defaultMaxOutputBytes  int64 = 10 * 1_024 * 1_024
)

// Config contains all runtime service settings.
type Config struct {
	Addr                string
	MMDCPath            string
	PuppeteerConfigPath string
	MermaidConfigPath   string
	MaxDiagramBytes     int64
	MaxOutputBytes      int64
	RenderConcurrency   int
	MaxInFlight         int
	RenderTimeout       time.Duration
	ShutdownTimeout     time.Duration
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
	renderConcurrency, err := positiveInt("RENDER_CONCURRENCY", 2)
	if err != nil {
		return Config{}, err
	}
	maxInFlight, err := positiveInt("MAX_IN_FLIGHT", 16)
	if err != nil {
		return Config{}, err
	}
	if maxInFlight < renderConcurrency {
		return Config{}, fmt.Errorf("MAX_IN_FLIGHT must be at least RENDER_CONCURRENCY")
	}
	renderTimeout, err := positiveDuration("RENDER_TIMEOUT", 20*time.Second)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Addr:                addr,
		MMDCPath:            envOrDefault("MMDC_PATH", "mmdc"),
		PuppeteerConfigPath: envOrDefault("MMDC_PUPPETEER_CONFIG", ""),
		MermaidConfigPath:   envOrDefault("MMDC_MERMAID_CONFIG", ""),
		MaxDiagramBytes:     maxDiagramBytes,
		MaxOutputBytes:      maxOutputBytes,
		RenderConcurrency:   renderConcurrency,
		MaxInFlight:         maxInFlight,
		RenderTimeout:       renderTimeout,
		ShutdownTimeout:     10 * time.Second,
	}, nil
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

func positiveDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := envOrDefault(key, fallback.String())
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return parsed, nil
}
