// Command hostd is the per-host migration agent: it serves the REST
// contract in internal/api and drives Firecracker microVMs through
// internal/fc.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/hostd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8080", "address to listen on")
	fcBin := flag.String("fc-bin", "/usr/local/bin/firecracker", "path to the firecracker binary")
	snapshotsDir := flag.String("snapshots-dir", "/snapshots", "root directory for VM snapshot artifacts")
	runDir := flag.String("run-dir", "/run/fc", "directory for firecracker API sockets and logs")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := os.MkdirAll(*snapshotsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", *snapshotsDir, err)
	}
	if err := os.MkdirAll(*runDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", *runDir, err)
	}

	srv := hostd.NewServer(hostd.Config{
		FCBin:        *fcBin,
		SnapshotsDir: *snapshotsDir,
		RunDir:       *runDir,
		Logger:       logger,
	})

	httpServer := &http.Server{
		Addr:    *listen,
		Handler: srv.Routes(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("hostd listening", "addr", *listen, "fc_bin", *fcBin, "snapshots_dir", *snapshotsDir, "run_dir", *runDir)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen and serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}

	srv.KillAll()
	logger.Info("all vms killed, exiting")
	return nil
}
