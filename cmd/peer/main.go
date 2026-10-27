// Owner: Rishav Sarkar (Download Engine & Concurrency)

// Command peer joins a ShardSwarm swarm. If the complete file is already in
// -dir it seeds; otherwise it downloads the file and then keeps seeding.
//
//	peer -meta FILE.meta.json [-dir DIR] [-tracker URL] [-listen HOST:PORT]
//	     [-strategy rarest|random|sequential] [-pipeline N] [-choke tft|off|freerider]
//	     [-up-limit BYTES_PER_SEC] [-metrics FILE.csv [-run-id ID] [-seeders N] [-swarm-size N]] [-debug]
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/metrics"
	"github.com/sneharam543/shardswarm/internal/peer"
	"github.com/sneharam543/shardswarm/internal/piece"
	"github.com/sneharam543/shardswarm/internal/tracker"
)

type config struct {
	metaPath, dir, trackerURL, listen, strategy, choke string
	pipeline                                           int
	upLimit                                            int64
	metricsPath, runID                                 string
	seeders, swarmSize                                 int // experiment labels for the metrics file only
}

func main() {
	var cfg config
	flag.StringVar(&cfg.metaPath, "meta", "", "metainfo file (required)")
	flag.StringVar(&cfg.dir, "dir", ".", "download directory; a complete file here is seeded")
	flag.StringVar(&cfg.trackerURL, "tracker", "http://127.0.0.1:8080", "tracker base URL")
	// Loopback by default; Docker sets -listen :6881 on its private network.
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:6881", "address for incoming peer connections")
	flag.StringVar(&cfg.strategy, "strategy", "rarest", "piece selection: rarest, random or sequential")
	flag.IntVar(&cfg.pipeline, "pipeline", 5, "outstanding block requests per peer")
	flag.StringVar(&cfg.choke, "choke", "tft", "upload policy: tft (tit-for-tat), off (unchoke all) or freerider (never upload)")
	flag.Int64Var(&cfg.upLimit, "up-limit", 0, "upload cap in bytes per second for all connections together; 0 = unlimited")
	flag.StringVar(&cfg.metricsPath, "metrics", "", "append measurements to this CSV file (off if empty)")
	flag.StringVar(&cfg.runID, "run-id", "", "run label in the metrics file (default: start time)")
	flag.IntVar(&cfg.seeders, "seeders", 0, "seeder count of this experiment, recorded in the metrics file")
	flag.IntVar(&cfg.swarmSize, "swarm-size", 0, "swarm size of this experiment, recorded in the metrics file")
	debug := flag.Bool("debug", false, "log debug events (strikes, rate limiting, rejected handshakes)")
	flag.Parse()
	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		slog.Error("peer stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	start := time.Now()
	if cfg.metaPath == "" {
		return errors.New("-meta is required")
	}
	if cfg.pipeline < 1 {
		return fmt.Errorf("-pipeline must be at least 1, got %d", cfg.pipeline)
	}
	if cfg.upLimit < 0 {
		return fmt.Errorf("-up-limit must be 0 (unlimited) or positive, got %d", cfg.upLimit)
	}
	strategy, err := piece.ParseStrategy(cfg.strategy)
	if err != nil {
		return err
	}
	choke, err := piece.ParseChokePolicy(cfg.choke)
	if err != nil {
		return err
	}
	meta, err := metainfo.LoadFile(cfg.metaPath, metainfo.DefaultMaxLength)
	if err != nil {
		return err
	}
	store, have, err := piece.OpenStore(cfg.dir, meta)
	if err != nil {
		return err
	}
	defer store.Close()

	var id [20]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	shortID := hex.EncodeToString(id[:4])

	var rec *metrics.Recorder // nil records nothing
	if cfg.metricsPath != "" {
		if cfg.runID == "" {
			cfg.runID = start.Format("20060102-150405")
		}
		rec, err = metrics.Open(cfg.metricsPath, metrics.Run{
			ID: cfg.runID, PeerID: shortID, Strategy: string(strategy), Choking: string(choke),
			Seeders: cfg.seeders, SwarmSize: cfg.swarmSize, UpLimit: cfg.upLimit,
		})
		if err != nil {
			return err
		}
		defer rec.Close()
	}
	engine := piece.NewEngine(meta, store, have, piece.Config{
		Strategy: strategy, Pipeline: cfg.pipeline, Choke: choke, OnPiece: rec.Piece,
	})

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := peer.NewManager(ctx, meta, id, engine)
	m.LimitUpload(cfg.upLimit)
	go func() {
		if err := m.Serve(ln); err != nil {
			slog.Error("listener failed", "err", err)
			cancel()
		}
	}()
	go engine.Run(ctx)
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		report(ctx, engine, rec, start)
	}()

	slog.Info("peer started", "id", shortID, "file", meta.Info.Name, "listen", ln.Addr().String(),
		"strategy", strategy, "choke", choke, "up_limit", cfg.upLimit, "left", engine.Stats().Left)
	announceLoop(ctx, cfg.trackerURL, m, engine, tracker.AnnounceParams{
		InfoHash: meta.InfoHashBytes(),
		PeerID:   id,
		Port:     ln.Addr().(*net.TCPAddr).Port,
	})
	m.Wait()
	<-reported // the reporter must stop writing before rec is closed
	return nil
}

// announceLoop reports to the tracker and dials the peers it returns until
// ctx ends, then announces "stopped".
func announceLoop(ctx context.Context, base string, m *peer.Manager, e *piece.Engine, p tracker.AnnounceParams) {
	done := e.Done()
	if e.Stats().Left == 0 {
		done = nil // started as a seeder: nothing to announce as completed
	}
	p.Event = "started"
	for {
		p.Left = e.Stats().Left
		wait := 5 * time.Second // retry delay after a failed announce
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		interval, peers, err := tracker.Announce(reqCtx, base, p)
		cancel()
		if err != nil {
			slog.Warn("announce failed", "err", err)
		} else {
			p.Event, wait = "", interval
			for _, c := range peers {
				m.Dial(c.Addr, c.ID)
			}
		}

		select {
		case <-ctx.Done():
			// Best effort, so the tracker drops us before our entry expires.
			stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			p.Event, p.Left = "stopped", e.Stats().Left
			_, _, _ = tracker.Announce(stopCtx, base, p)
			cancel()
			return
		case <-done:
			done, p.Event = nil, "completed"
		case <-time.After(wait):
		}
	}
}

// report samples progress every second: one CSV row, plus one log line
// whenever data moved, so each peer's terminal shows what it is doing.
func report(ctx context.Context, e *piece.Engine, rec *metrics.Recorder, start time.Time) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	prev, prevAt := e.Stats(), time.Now()
	done := e.Done()
	if prev.Left == 0 {
		done = nil
		slog.Info("seeding")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			elapsed := time.Since(start)
			slog.Info("download complete", "elapsed", elapsed.Round(time.Millisecond))
			rec.Complete(elapsed)
			done = nil
		case now := <-tick.C:
			s := e.Stats()
			secs := now.Sub(prevAt).Seconds()
			down := float64(s.Downloaded-prev.Downloaded) / secs
			up := float64(s.Uploaded-prev.Uploaded) / secs
			rec.Rate(metrics.Sample{BytesDown: s.Downloaded, BytesUp: s.Uploaded, DownBps: down, UpBps: up,
				Have: s.Have, Total: s.Pieces}, s.Left == 0)
			if s.Downloaded != prev.Downloaded || s.Uploaded != prev.Uploaded {
				slog.Info("progress", "pieces", fmt.Sprintf("%d/%d", s.Have, s.Pieces),
					"done", fmt.Sprintf("%.0f%%", 100*float64(s.Have)/float64(max(s.Pieces, 1))),
					"down", mibps(down), "up", mibps(up), "peers", s.Peers)
			}
			prev, prevAt = s, now
		}
	}
}

func mibps(bytesPerSec float64) string { return fmt.Sprintf("%.2fMiB/s", bytesPerSec/(1<<20)) }
