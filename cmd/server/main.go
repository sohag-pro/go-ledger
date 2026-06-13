// Command server runs the go-ledger HTTP API.
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

	"github.com/sohag-pro/go-ledger/internal/api"
	"github.com/sohag-pro/go-ledger/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	web.Register(mux)
	// api.New registers /healthz (a typed huma operation), /openapi.json,
	// /openapi.yaml, and /schemas/. RegisterPlayground serves the Scalar UI.
	api.New(mux)
	api.RegisterPlayground(mux)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting server", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
