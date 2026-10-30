// Command server runs the rockets service.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rockets-service/internal/api"
	"rockets-service/internal/service"
	"rockets-service/internal/storage/sqlite"
)

func main() {
	addr := flag.String("addr", ":8088", "HTTP listen address")
	dbPath := flag.String("db", "rockets.db", "SQLite database file (created if missing)")
	syncMode := flag.String("sync", string(sqlite.SyncNormal),
		"SQLite durability: normal (survives a process crash; fast) or full (survives power loss; ~100x slower)")
	maxPending := flag.Int("max-pending", service.DefaultMaxPending, "max early messages buffered per rocket")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		slog.Error("invalid -log-level", "err", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(*addr, *dbPath, sqlite.Sync(*syncMode), *maxPending, logger); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(addr, dbPath string, syncMode sqlite.Sync, maxPending int, logger *slog.Logger) error {
	db, err := sqlite.Open(dbPath, syncMode)
	if err != nil {
		return err
	}
	defer db.Close() // after the HTTP server has drained in-flight requests

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.New(service.New(db, logger, maxPending), logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind before logging, so "listening" is only printed once it is true.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", ln.Addr().String(), "db", dbPath, "sync", syncMode, "maxPending", maxPending)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
