// Command server runs the rockets service.
package main

import (
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"rockets-service/internal/api"
	"rockets-service/internal/service"
	"rockets-service/internal/storage/sqlite"
)

func main() {
	addr := flag.String("addr", ":8088", "HTTP listen address")
	dbPath := flag.String("db", "rockets.db", "SQLite database file (created if missing)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*addr, *dbPath, logger); err != nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(addr, dbPath string, logger *slog.Logger) error {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	srv := &http.Server{
		Handler:           api.New(service.New(db, logger), logger),
		ReadHeaderTimeout: 5 * time.Second, // don't let a slow client hold a connection forever
	}

	// Bind before logging, so "listening" is only printed once it is true.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", ln.Addr().String(), "db", dbPath)
	return srv.Serve(ln)
}
