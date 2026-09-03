package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/belazy/mermaid-mcp/internal/admission"
	"github.com/belazy/mermaid-mcp/internal/assetstore"
	"github.com/belazy/mermaid-mcp/internal/config"
	"github.com/belazy/mermaid-mcp/internal/render"
	"github.com/belazy/mermaid-mcp/internal/server"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	if err := protectProcessSecrets(); err != nil {
		return fmt.Errorf("protect process credentials: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var missAdmitter render.MissAdmitter
	if cfg.ClusterAdmissionURL != "" {
		missAdmitter, err = admission.NewClient(cfg.ClusterAdmissionURL, cfg.ClusterAdmissionToken)
		if err != nil {
			return fmt.Errorf("configure cluster admission: %w", err)
		}
	}

	workerPool, err := render.NewWorkerPool(context.Background(), render.WorkerPoolConfig{
		NodePath:              cfg.NodePath,
		ScriptPath:            cfg.WorkerScriptPath,
		ChromiumPath:          cfg.ChromiumPath,
		MaxDiagramBytes:       cfg.MaxDiagramBytes,
		MaxOutputBytes:        cfg.MaxOutputBytes,
		Workers:               cfg.RenderWorkers,
		QueueSize:             cfg.RenderQueueSize,
		Timeout:               cfg.RenderTimeout,
		MaxRendersPerWorker:   cfg.WorkerMaxRenders,
		MaxWorkerAge:          cfg.WorkerMaxAge,
		MaxWorkerRSSBytes:     cfg.WorkerMaxRSSBytes,
		IsolateNetwork:        cfg.WorkerNetworkIsolation,
		IsolationLauncherPath: cfg.WorkerIsolationLauncher,
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := workerPool.Close(); closeErr != nil {
			logger.Error("close renderer pool", "error", closeErr)
		}
	}()

	renderer, err := render.NewCache(workerPool, render.CacheConfig{
		BundleID:              cfg.RendererBundleID,
		MaxBytes:              cfg.CacheMaxBytes,
		MaxEntries:            cfg.CacheMaxEntries,
		MaxOutputBytes:        cfg.MaxOutputBytes,
		TTL:                   cfg.CacheTTL,
		RejectionTTL:          cfg.CacheRejectionTTL,
		FillTimeout:           cfg.RenderTimeout,
		MaxWaiters:            cfg.MaxInFlight,
		MaxConcurrentFills:    cfg.RenderWorkers,
		MaxQueuedFills:        cfg.RenderQueueSize,
		MissesPerSecond:       cfg.RenderMissRPS,
		MissBurst:             cfg.RenderMissBurst,
		ClientMissesPerSecond: cfg.ClientRenderMissRPS,
		ClientMissBurst:       cfg.ClientRenderMissBurst,
		MaxClients:            cfg.ClientLimiterEntries,
		MissAdmitter:          missAdmitter,
	})
	if err != nil {
		return err
	}

	var assets server.AssetStore
	if cfg.AssetStorageConfigured() {
		assets, err = assetstore.NewR2(assetstore.Config{
			Endpoint:        cfg.R2Endpoint,
			AccessKeyID:     cfg.R2AccessKeyID,
			SecretAccessKey: cfg.R2SecretAccessKey,
			Bucket:          cfg.R2Bucket,
			PublicBaseURL:   cfg.AssetPublicBaseURL,
			ExistenceTTL:    cfg.AssetExistenceTTL,
			MaxMemoEntries:  cfg.AssetMemoEntries,
			MaxObjectBytes:  cfg.MaxOutputBytes,
		})
		if err != nil {
			return err
		}
	}

	handler, err := server.New(renderer, server.Options{
		Version:          version,
		RendererBundleID: cfg.RendererBundleID,
		MaxDiagramBytes:  cfg.MaxDiagramBytes,
		MaxInFlight:      cfg.MaxInFlight,
		ClientIPHeader:   cfg.TrustedClientIPHeader,
		Ready:            workerPool.Ready,
		CacheStats:       renderer.Stats,
		AssetStore:       assets,
		AssetHMACKey:     cfg.AssetHMACKey,
		Logger:           logger,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RenderTimeout + 65*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1_024,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("service listening", "addr", cfg.Addr, "version", version, "render_workers", cfg.RenderWorkers)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("service shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
		return err
	}
	return nil
}
