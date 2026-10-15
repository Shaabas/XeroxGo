package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xeroxgo/cloud-backend/internal/config"
	"xeroxgo/cloud-backend/internal/httpapi"
)

var (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(logger); err != nil {
		logger.Error("cloud backend terminated", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	return serveHTTP(ctx, cfg.HTTPAddr, logger)
}

func serveHTTP(ctx context.Context, addr string, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	return serve(ctx, listener, httpapi.NewHandler(), logger)
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if handler == nil {
		handler = httpapi.NewHandler()
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    1 << 20,
	}

	serverErrors := make(chan error, 1)

	go func() {
		serverErrors <- server.Serve(listener)
	}()

	logger.Info("HTTP server listening", "addr", listener.Addr().String())

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP server failed: %w", err)

	case <-ctx.Done():
		logger.Info("HTTP server shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		shutdownTimeout,
	)
	defer cancel()

	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		logger.Error(
			"graceful shutdown failed; closing server",
			"error", shutdownErr,
		)
		_ = server.Close()
	}

	serveErr := <-serverErrors
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server failed during shutdown: %w", serveErr)
	}

	if shutdownErr != nil {
		return fmt.Errorf("HTTP server shutdown failed: %w", shutdownErr)
	}

	logger.Info("HTTP server stopped")
	return nil
}
