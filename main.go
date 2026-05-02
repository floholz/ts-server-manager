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

	"github.com/floholz/ts-server-manager/internal/api"
	"github.com/floholz/ts-server-manager/internal/config"
	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
	"github.com/floholz/ts-server-manager/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	if err := runServe(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func runServe() error {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tsClient := ts.New(ts.Config{
		Host: cfg.TSHost, Port: cfg.TSPort,
		User: cfg.TSUser, Password: cfg.TSPassword,
		SID: cfg.TSSID,
	}, logger)
	tsClient.Start(ctx)

	hubClient := dockerhub.New(dockerhub.Config{
		Repo: cfg.DockerHubRepo,
		TTL:  cfg.UpdateCheckTTL,
	})

	mux := http.NewServeMux()
	api.Register(mux, tsClient, hubClient, logger)

	handler := api.RecoverAndLog(logger)(mux)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http.listen", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown.signal_received")
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http.shutdown", "err", err.Error())
	}
	logger.Info("shutdown.done")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With(
		slog.String("service.name", "ts-server-manager"),
		slog.String("service.version", version.Version),
	)
}

// runHealthcheck issues a single GET /readyz against the local server and
// returns 0 on 200, 1 otherwise. Used by the Dockerfile HEALTHCHECK.
func runHealthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":9988"
	}
	// HTTP_ADDR may be ":9988" or "0.0.0.0:9988"; normalise to a localhost URL.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid HTTP_ADDR:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/readyz", net.JoinHostPort(host, port))

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
