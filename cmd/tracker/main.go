// Owner: Sneha Ram (Metainfo & Tracker)

// Command tracker runs the ShardSwarm HTTP tracker.
//
//	tracker [-addr HOST:PORT] [-interval DURATION]
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sneharam543/shardswarm/internal/tracker"
)

func main() {
	// Loopback by default so a stray run is not reachable from the network;
	// Docker sets -addr :8080 on its private network.
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	interval := flag.Duration("interval", 15*time.Second, "announce interval; peers expire after two missed intervals")
	flag.Parse()
	if *interval < time.Second {
		slog.Error("interval must be at least 1s", "interval", *interval)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tr := tracker.New(*interval)
	go tr.Run(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           tr.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("tracker listening", "addr", *addr, "interval", *interval)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("tracker stopped", "err", err)
		os.Exit(1)
	}
	slog.Info("tracker shut down")
}
