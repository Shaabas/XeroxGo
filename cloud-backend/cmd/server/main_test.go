package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeHTTPGracefulShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on ephemeral port: %v", err)
	}
	addr := listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	errCh := make(chan error, 1)
	go func() {
		errCh <- serve(ctx, listener, nil, logger)
	}()

	// Confirm the server is accepting requests and healthy before cancelling
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		t.Fatalf("failed to query /healthz on running server: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Trigger graceful shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve() returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() timed out waiting for shutdown")
	}

	// Confirm connections are no longer accepted
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected connection to fail after server shutdown, but it succeeded")
	}
}

func TestServeHTTPBindFailure(t *testing.T) {
	// Occupy a port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = serveHTTP(ctx, addr, logger)
	if err == nil {
		t.Fatal("serveHTTP() expected error for already bound address, got nil")
	}
}

func TestServeHTTPBoundedShutdown(t *testing.T) {
	// Temporarily shorten shutdownTimeout to test forceful closure behavior
	origTimeout := shutdownTimeout
	shutdownTimeout = 100 * time.Millisecond
	defer func() {
		shutdownTimeout = origTimeout
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	inFlight := make(chan struct{})
	slowHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		// Stalls longer than the shortened shutdownTimeout
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusOK)
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- serve(ctx, listener, slowHandler, logger)
	}()

	// Start a request that will block in the handler
	go func() {
		resp, _ := http.Get(fmt.Sprintf("http://%s/slow", addr))
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()

	// Wait until the handler has begun execution
	<-inFlight

	// Request shutdown while the request is actively in-flight
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from bounded shutdown timeout, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded in error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve() timed out waiting for bounded shutdown")
	}
}

func TestRunInvalidConfig(t *testing.T) {
	t.Setenv("HTTP_ADDR", "invalid-port")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := run(logger)
	if err == nil {
		t.Fatal("run() expected error with invalid HTTP_ADDR, got nil")
	}
}
