package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/logdepot/server/handler"
	"github.com/logdepot/server/scanner"
	"github.com/logdepot/server/state"
)

func main() {
	// Structured logging to stderr.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	addr := envOrDefault("LISTEN_ADDR", ":8080")
	stateDir := envOrDefault("STATE_DIR", "/var/lib/logdepot")

	store, err := state.NewStore(stateDir)
	if err != nil {
		slog.Error("failed to create state store", "error", err, "dir", stateDir)
		os.Exit(1)
	}

	mgr, err := scanner.NewManager(store)
	if err != nil {
		slog.Error("failed to create scan manager", "error", err)
		os.Exit(1)
	}

	// Recover any persisted jobs from a previous run.
	if err := mgr.Recover(); err != nil {
		slog.Error("recovery failed", "error", err)
		// Non-fatal: the server can still accept new jobs.
	}

	h := handler.New(mgr)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Mount("/", h.Routes())

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutting down...")

	// Give in-flight requests up to 15 seconds to complete.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Stop all scan jobs and drain the notifier first.
	mgr.Shutdown()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	slog.Info("server stopped")
}

// requestLogger is a lightweight Chi middleware that logs each request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", ww.BytesWritten(),
		)
	})
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
