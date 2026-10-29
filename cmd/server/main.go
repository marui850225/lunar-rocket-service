// Command server runs the rockets service.
package main

import (
	"flag"
	"log/slog"
	"os"
)

func main() {
	addr := flag.String("addr", ":8088", "HTTP listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	logger.Info("rockets service: not wired yet", "addr", *addr)
}
