// Tests for the peer command: config validation, and a six-process swarm (the test binary re-runs itself as each peer) against a live tracker.
// Owner: Rishav Sarkar (Download Engine & Concurrency)

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/metrics"
	"github.com/sneharam543/shardswarm/internal/tracker"
)

// The test binary doubles as the peer binary: run with this variable set,
// it executes main instead of the tests. Each peer is then a real process.
const runPeerEnv = "SHARDSWARM_RUN_PEER"

func TestMain(m *testing.M) {
	if os.Getenv(runPeerEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRun_RejectsBadConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  config
	}{
		{"missing meta", config{pipeline: 5, strategy: "rarest", choke: "tft"}},
		{"bad pipeline", config{metaPath: "x", pipeline: 0, strategy: "rarest", choke: "tft"}},
		{"bad strategy", config{metaPath: "x", pipeline: 5, strategy: "fastest", choke: "tft"}},
		{"bad choke policy", config{metaPath: "x", pipeline: 5, strategy: "rarest", choke: "greedy"}},
		{"negative up-limit", config{metaPath: "x", pipeline: 5, strategy: "rarest", choke: "tft", upLimit: -1}},
		{"meta not found", config{metaPath: filepath.Join(t.TempDir(), "nope"), pipeline: 5, strategy: "rarest", choke: "tft"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := run(context.Background(), tt.cfg); err == nil {
				t.Error("run accepted a bad config")
			}
		})
	}
}

// One seeder and five leechers as six independent OS processes, found
// through a live tracker. Every leecher must end with a byte-identical file,
// and the tracker must count all six as complete.
func TestSwarm_SixProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts six peer processes")
	}
	tr := tracker.New(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.Run(ctx)
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	data := make([]byte, 2<<20)
	rand.New(rand.NewSource(6)).Read(data)
	seedDir := t.TempDir()
	src := filepath.Join(seedDir, "payload.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := metainfo.FromFile(src, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(t.TempDir(), "payload.bin.meta.json")
	f, err := os.Create(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	logDir := t.TempDir()
	startPeer := func(name, dir string) {
		logFile, err := os.Create(filepath.Join(logDir, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-meta", metaPath, "-dir", dir,
			"-tracker", srv.URL, "-listen", "127.0.0.1:0", "-strategy", "rarest",
			"-metrics", filepath.Join(logDir, name+".csv"), "-run-id", "swarm-test",
			"-seeders", "1", "-swarm-size", "6")
		cmd.Env = append(os.Environ(), runPeerEnv+"=1")
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cmd.Process.Kill()
			cmd.Wait()
			logFile.Close()
			if t.Failed() {
				out, _ := os.ReadFile(logFile.Name())
				t.Logf("--- %s log ---\n%s", name, out)
			}
		})
	}

	startPeer("seeder", seedDir)
	var leecherDirs, leecherCSVs []string
	for i := 1; i <= 5; i++ {
		dir := t.TempDir()
		name := "leecher" + string(rune('0'+i))
		leecherDirs = append(leecherDirs, dir)
		leecherCSVs = append(leecherCSVs, filepath.Join(logDir, name+".csv"))
		startPeer(name, dir)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		if allMatch(leecherDirs, data) && scrapeComplete(t, srv.URL, meta) == 6 &&
			allMetricsComplete(leecherCSVs, meta.NumPieces()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("swarm did not complete within 60s (tracker counts %d complete)", scrapeComplete(t, srv.URL, meta))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// allMetricsComplete reports whether every peer's CSV has the schema, one
// row per verified piece, and a completion row with its total time.
func allMetricsComplete(paths []string, pieces int) bool {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		rows, err := csv.NewReader(f).ReadAll()
		f.Close()
		if err != nil || len(rows) == 0 || strings.Join(rows[0], ",") != strings.Join(metrics.Header, ",") {
			return false
		}
		pieceRows, complete := 0, false
		for _, r := range rows[1:] {
			switch r[3] { // event
			case "piece":
				pieceRows++
			case "complete":
				complete = r[13] != "" && r[0] == "swarm-test" // total_time_ms, run_id
			}
		}
		if pieceRows != pieces || !complete {
			return false
		}
	}
	return true
}

func allMatch(dirs []string, data []byte) bool {
	for _, dir := range dirs {
		got, err := os.ReadFile(filepath.Join(dir, "payload.bin"))
		if err != nil || !bytes.Equal(got, data) {
			return false
		}
	}
	return true
}

func scrapeComplete(t *testing.T, base string, meta *metainfo.MetaInfo) int {
	t.Helper()
	ih := meta.InfoHashBytes()
	resp, err := http.Get(base + "/scrape?info_hash=" + hex.EncodeToString(ih[:]))
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var stats struct{ Complete int }
	if json.NewDecoder(resp.Body).Decode(&stats) != nil {
		return -1
	}
	return stats.Complete
}
