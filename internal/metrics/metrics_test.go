// Tests for metrics: the CSV schema, appending across runs, concurrent writers, and the nil recorder.
// Owner: Swayam Kanjilal (Choking & Experiments)

package metrics

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll() // also checks every row has the same column count
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func col(row []string, name string) string {
	for i, h := range Header {
		if h == name {
			return row[i]
		}
	}
	panic("no column " + name)
}

func TestRecorder_WritesSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.csv")
	run := Run{ID: "r1", PeerID: "ab12", Strategy: "rarest", Choking: "freerider", Seeders: 2, SwarmSize: 6, UpLimit: 1 << 20}
	r, err := Open(path, run)
	if err != nil {
		t.Fatal(err)
	}
	r.Piece(7, 262144)
	r.Rate(Sample{BytesDown: 100, BytesUp: 50, DownBps: 1000.4, UpBps: 0, Have: 1, Total: 4}, false)
	r.Complete(1500 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	rows := readCSV(t, path)
	if len(rows) != 4 || len(rows[0]) != len(Header) {
		t.Fatalf("got %d rows of %d columns, want 4 of %d", len(rows), len(rows[0]), len(Header))
	}
	piece, rate, done := rows[1], rows[2], rows[3]
	checks := []struct {
		row        []string
		name, want string
	}{
		{piece, "event", "piece"}, {piece, "piece_index", "7"}, {piece, "piece_bytes", "262144"},
		{rate, "event", "rate"}, {rate, "bytes_down", "100"}, {rate, "down_bps", "1000"},
		{rate, "pieces_have", "1"}, {rate, "complete", "false"},
		{done, "event", "complete"}, {done, "complete", "true"}, {done, "total_time_ms", "1500"},
		{done, "run_id", "r1"}, {done, "peer_id", "ab12"}, {done, "strategy", "rarest"},
		{done, "seeders", "2"}, {done, "swarm_size", "6"}, {done, "bw_limit_bps", "1048576"},
		{done, "free_rider", "true"},
	}
	for _, c := range checks {
		if got := col(c.row, c.name); got != c.want {
			t.Errorf("%s %s = %q, want %q", col(c.row, "event"), c.name, got, c.want)
		}
	}
}

func TestRecorder_AppendsWithOneHeader(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.csv")
	for _, id := range []string{"run-a", "run-b"} {
		r, err := Open(path, Run{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		r.Complete(time.Second)
		r.Close()
	}
	rows := readCSV(t, path)
	if len(rows) != 3 || col(rows[1], "run_id") != "run-a" || col(rows[2], "run_id") != "run-b" {
		t.Errorf("rows = %v, want one header then run-a, run-b", rows)
	}
}

func TestRecorder_ConcurrentWrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m.csv")
	r, err := Open(path, Run{ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				r.Piece(i, 1)
				r.Rate(Sample{}, false)
			}
		}()
	}
	wg.Wait()
	r.Close()
	if rows := readCSV(t, path); len(rows) != 1+8*200 {
		t.Errorf("got %d rows, want %d", len(rows), 1+8*200)
	}
}

func TestRecorder_NilIsNoOp(t *testing.T) {
	t.Parallel()
	var r *Recorder
	r.Piece(1, 1)
	r.Rate(Sample{}, true)
	r.Complete(time.Second)
	if err := r.Close(); err != nil {
		t.Error(err)
	}
}
