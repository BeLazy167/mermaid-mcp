package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	renderer, err := render.NewMMDC(render.MMDCConfig{
		Path:                cfg.MMDCPath,
		PuppeteerConfigPath: cfg.PuppeteerConfigPath,
		MermaidConfigPath:   cfg.MermaidConfigPath,
		MaxDiagramBytes:     cfg.MaxDiagramBytes,
		MaxOutputBytes:      cfg.MaxOutputBytes,
		Concurrency:         cfg.RenderConcurrency,
		Timeout:             cfg.RenderTimeout,
	})
	if err != nil {
		return err
	}

	handler, err := server.New(renderer, server.Options{
		Version:         version,
		MaxDiagramBytes: cfg.MaxDiagramBytes,
		MaxInFlight:     cfg.MaxInFlight,
		Logger:          logger,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.RenderTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1_024,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("service listening", "addr", cfg.Addr, "version", version)
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
