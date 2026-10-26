// Owner: Swayam Kanjilal (Choking & Experiments)

// Package metrics appends one peer's measurements to a CSV file for the
// experiments. The columns are listed in Header; units are in the names.
package metrics

import (
	"encoding/csv"
	"os"
	"strconv"
	"sync"
	"time"
)

// Header is the CSV schema. Every row has every column; a value that does
// not apply to an event is empty.
var Header = []string{
	"run_id", "timestamp_ms", "peer_id", "event",
	"piece_index", "piece_bytes", // event "piece": one verified piece
	"bytes_down", "bytes_up", "down_bps", "up_bps", "pieces_have", "pieces_total", // event "rate": 1 s sample
	"complete", "total_time_ms", // "complete" true from the first sample after completion; total_time_ms on event "complete"
	"strategy", "choking", "seeders", "swarm_size", "bw_limit_bps", "free_rider", // run configuration
}

// Run labels every row of one peer's file.
type Run struct {
	ID, PeerID, Strategy, Choking string
	Seeders, SwarmSize            int
	UpLimit                       int64 // bytes per second, 0 = unlimited
}

// Sample is one rate measurement.
type Sample struct {
	BytesDown, BytesUp int64
	DownBps, UpBps     float64
	Have, Total        int
}

// Recorder writes rows for one peer. A nil *Recorder records nothing, so
// callers need no checks when metrics are off. Safe for concurrent use.
type Recorder struct {
	mu            sync.Mutex
	f             *os.File
	w             *csv.Writer
	runID, peerID string
	config        []string // the run configuration columns, formatted once
}

// Open appends to path, writing the header if the file is new or empty, so
// repeated runs can share one file and be told apart by run_id.
func Open(path string, run Run) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		f:      f,
		w:      csv.NewWriter(f),
		runID:  run.ID,
		peerID: run.PeerID,
		config: []string{run.Strategy, run.Choking, strconv.Itoa(run.Seeders), strconv.Itoa(run.SwarmSize),
			strconv.FormatInt(run.UpLimit, 10), strconv.FormatBool(run.Choking == "freerider")},
	}
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		r.w.Write(Header)
	}
	return r, nil
}

// Piece records a verified piece.
func (r *Recorder) Piece(index int, size int64) {
	r.row("piece", strconv.Itoa(index), strconv.FormatInt(size, 10), "", "", "", "", "", "", "", "")
}

// Rate records a sample; complete says whether the download is finished.
func (r *Recorder) Rate(s Sample, complete bool) {
	r.row("rate", "", "",
		strconv.FormatInt(s.BytesDown, 10), strconv.FormatInt(s.BytesUp, 10),
		strconv.FormatFloat(s.DownBps, 'f', 0, 64), strconv.FormatFloat(s.UpBps, 'f', 0, 64),
		strconv.Itoa(s.Have), strconv.Itoa(s.Total), strconv.FormatBool(complete), "")
	r.flush()
}

// Complete records the moment the whole file was verified.
func (r *Recorder) Complete(elapsed time.Duration) {
	r.row("complete", "", "", "", "", "", "", "", "", "true", strconv.FormatInt(elapsed.Milliseconds(), 10))
	r.flush()
}

// Close flushes and closes the file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w.Flush()
	return r.f.Close()
}

func (r *Recorder) row(event string, cols ...string) {
	if r == nil {
		return
	}
	rec := append([]string{r.runID, strconv.FormatInt(time.Now().UnixMilli(), 10), r.peerID, event}, cols...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w.Write(append(rec, r.config...))
}

func (r *Recorder) flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w.Flush()
}
