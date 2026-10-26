// Tests for the tracker server: announce and scrape rules, invalid input, duplicate IDs, expiry, capacity, and concurrent clients.
// Owner: Sneha Ram (Metainfo & Tracker)

package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const interval = 15 * time.Second

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
)

func peerID(n int) string { return fmt.Sprintf("%040x", n) }

// fakeClock lets tests move time forward without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestTracker() (*Tracker, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	tr := New(interval)
	tr.now = clock.now
	return tr, clock
}

type announceResp struct {
	Interval int    `json:"interval"`
	Peers    []Peer `json:"peers"`
	Error    string `json:"error"`
}

// get calls the handler as a client at remote (host:port) and decodes the JSON body into out.
func get(t *testing.T, h http.Handler, target, remote string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusMethodNotAllowed && ct != "application/json" {
		t.Errorf("%s: Content-Type = %q, want application/json", target, ct)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: decode %q: %v", target, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func announceURL(infoHash, id string, port int, left int64, event string) string {
	q := url.Values{}
	q.Set("info_hash", infoHash)
	q.Set("peer_id", id)
	q.Set("port", fmt.Sprint(port))
	q.Set("left", fmt.Sprint(left))
	if event != "" {
		q.Set("event", event)
	}
	return "/announce?" + q.Encode()
}

func announce(t *testing.T, h http.Handler, infoHash string, n int, left int64, event string) (int, announceResp) {
	t.Helper()
	var resp announceResp
	ip := fmt.Sprintf("10.0.0.%d:5555", n%250+1)
	code := get(t, h, announceURL(infoHash, peerID(n), 6881, left, event), ip, &resp)
	return code, resp
}

func TestTracker_AnnounceRegistersAndDiscovers(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()

	code, resp := announce(t, h, hashA, 1, 100, "started")
	if code != http.StatusOK || len(resp.Peers) != 0 || resp.Interval != 15 {
		t.Fatalf("first announce: code %d, resp %+v", code, resp)
	}

	_, resp = announce(t, h, hashA, 2, 100, "started")
	want := Peer{PeerID: peerID(1), IP: "10.0.0.2", Port: 6881}
	if len(resp.Peers) != 1 || resp.Peers[0] != want {
		t.Fatalf("second peer got %+v, want [%+v]", resp.Peers, want)
	}

	_, resp = announce(t, h, hashA, 1, 100, "")
	if len(resp.Peers) != 1 || resp.Peers[0].PeerID != peerID(2) {
		t.Errorf("first peer re-announce got %+v, want peer 2", resp.Peers)
	}

	// A different swarm is isolated.
	_, resp = announce(t, h, hashB, 3, 100, "started")
	if len(resp.Peers) != 0 {
		t.Errorf("other swarm leaked peers: %+v", resp.Peers)
	}
}

func TestTracker_AnnounceRejectsInvalidParams(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()
	good := url.Values{
		"info_hash": {hashA}, "peer_id": {peerID(1)}, "port": {"6881"}, "left": {"0"},
	}
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"missing info_hash", "info_hash", ""},
		{"short info_hash", "info_hash", hashA[:63]},
		{"uppercase info_hash", "info_hash", strings.ToUpper(hashA)},
		{"non-hex info_hash", "info_hash", strings.Repeat("g", 64)},
		{"missing peer_id", "peer_id", ""},
		{"long peer_id", "peer_id", peerID(1) + "00"},
		{"port zero", "port", "0"},
		{"port too large", "port", "65536"},
		{"port not a number", "port", "http"},
		{"missing left", "left", ""},
		{"negative left", "left", "-1"},
		{"left overflow", "left", "99999999999999999999"},
		{"unknown event", "event", "paused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range good {
				q[k] = v
			}
			q.Set(tt.key, tt.value)
			var resp announceResp
			if code := get(t, h, "/announce?"+q.Encode(), "10.0.0.1:1", &resp); code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400", code)
			}
			if resp.Error == "" {
				t.Error("missing error message")
			}
		})
	}

	if code := get(t, h, "/announce?"+good.Encode(), "not-an-address", nil); code != http.StatusBadRequest {
		t.Errorf("bad RemoteAddr: code = %d, want 400", code)
	}
}

func TestTracker_AnnounceDuplicatePeerID(t *testing.T) {
	t.Parallel()
	tr, clock := newTestTracker()
	h := tr.Handler()
	id := peerID(7)

	if code := get(t, h, announceURL(hashA, id, 6881, 10, "started"), "10.0.0.1:1", nil); code != http.StatusOK {
		t.Fatalf("first announce code %d", code)
	}
	for _, tc := range []struct {
		name, remote string
		port         int
	}{
		{"other ip", "10.0.0.2:1", 6881},
		{"other port", "10.0.0.1:1", 6882},
	} {
		if code := get(t, h, announceURL(hashA, id, tc.port, 10, ""), tc.remote, nil); code != http.StatusConflict {
			t.Errorf("%s: code = %d, want 409", tc.name, code)
		}
	}

	// Once the original holder expires, the ID can be taken over.
	clock.advance(2*interval + time.Second)
	if code := get(t, h, announceURL(hashA, id, 6881, 10, ""), "10.0.0.2:1", nil); code != http.StatusOK {
		t.Errorf("takeover after expiry: code = %d, want 200", code)
	}
}

func TestTracker_AnnounceStopped(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()
	announce(t, h, hashA, 1, 0, "started")
	announce(t, h, hashA, 2, 0, "started")

	// Someone else sending "stopped" with peer 1's ID must not remove it.
	get(t, h, announceURL(hashA, peerID(1), 6881, 0, "stopped"), "10.9.9.9:1", nil)
	if _, resp := announce(t, h, hashA, 2, 0, ""); len(resp.Peers) != 1 {
		t.Fatalf("spoofed stop removed the peer: %+v", resp.Peers)
	}

	code, resp := announce(t, h, hashA, 1, 0, "stopped")
	if code != http.StatusOK || resp.Peers == nil || len(resp.Peers) != 0 {
		t.Errorf("stopped: code %d, peers %v, want 200 and []", code, resp.Peers)
	}
	if _, resp := announce(t, h, hashA, 2, 0, ""); len(resp.Peers) != 0 {
		t.Errorf("stopped peer still listed: %+v", resp.Peers)
	}
}

func TestTracker_AnnounceCapsPeerList(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()
	for n := 0; n < maxPeersReturned+20; n++ {
		announce(t, h, hashA, n, 1, "started")
	}
	_, resp := announce(t, h, hashA, 0, 1, "")
	if len(resp.Peers) != maxPeersReturned {
		t.Fatalf("got %d peers, want %d", len(resp.Peers), maxPeersReturned)
	}
	seen := map[string]bool{}
	for _, p := range resp.Peers {
		if p.PeerID == peerID(0) {
			t.Error("response includes the caller")
		}
		if seen[p.PeerID] {
			t.Errorf("duplicate peer %s", p.PeerID)
		}
		seen[p.PeerID] = true
	}
}

func TestTracker_AnnounceCapacity(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()

	for n := 0; n < maxPeersPerSwarm; n++ {
		if code, _ := announce(t, h, hashA, n, 1, ""); code != http.StatusOK {
			t.Fatalf("peer %d: code %d", n, code)
		}
	}
	if code, _ := announce(t, h, hashA, maxPeersPerSwarm, 1, ""); code != http.StatusServiceUnavailable {
		t.Errorf("full swarm: code = %d, want 503", code)
	}
	if code, _ := announce(t, h, hashA, 0, 0, ""); code != http.StatusOK {
		t.Errorf("existing peer in full swarm: code = %d, want 200", code)
	}

	for n := 1; n < maxSwarms; n++ {
		if code, _ := announce(t, h, fmt.Sprintf("%064x", n), 1, 1, ""); code != http.StatusOK {
			t.Fatalf("swarm %d: code %d", n, code)
		}
	}
	if code, _ := announce(t, h, fmt.Sprintf("%064x", maxSwarms), 1, 1, ""); code != http.StatusServiceUnavailable {
		t.Errorf("too many swarms: code = %d, want 503", code)
	}
}

func TestTracker_Scrape(t *testing.T) {
	t.Parallel()
	tr, clock := newTestTracker()
	h := tr.Handler()
	announce(t, h, hashA, 1, 0, "started")   // seeder
	announce(t, h, hashA, 2, 500, "started") // leecher
	announce(t, h, hashA, 3, 500, "started")
	announce(t, h, hashA, 3, 0, "completed")

	var stats map[string]int
	if code := get(t, h, "/scrape?info_hash="+hashA, "10.0.0.1:1", &stats); code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if stats["complete"] != 2 || stats["incomplete"] != 1 {
		t.Errorf("stats = %v, want complete 2, incomplete 1", stats)
	}

	// Peers past their deadline are not counted even before the sweeper runs.
	clock.advance(2*interval + time.Second)
	announce(t, h, hashA, 4, 9, "")
	get(t, h, "/scrape?info_hash="+hashA, "10.0.0.1:1", &stats)
	if stats["complete"] != 0 || stats["incomplete"] != 1 {
		t.Errorf("after expiry stats = %v, want complete 0, incomplete 1", stats)
	}

	if code := get(t, h, "/scrape?info_hash="+hashB, "10.0.0.1:1", nil); code != http.StatusNotFound {
		t.Errorf("unknown swarm: code = %d, want 404", code)
	}
	if code := get(t, h, "/scrape?info_hash=xyz", "10.0.0.1:1", nil); code != http.StatusBadRequest {
		t.Errorf("bad info_hash: code = %d, want 400", code)
	}
}

func TestTracker_Expire(t *testing.T) {
	t.Parallel()
	tr, clock := newTestTracker()
	h := tr.Handler()
	announce(t, h, hashA, 1, 0, "started")
	clock.advance(interval)
	announce(t, h, hashA, 2, 0, "started")

	clock.advance(interval + time.Second) // peer 1 is now 2*interval+1s old
	if _, resp := announce(t, h, hashA, 2, 0, ""); len(resp.Peers) != 0 {
		t.Errorf("expired peer still listed: %+v", resp.Peers)
	}

	clock.advance(3 * interval)
	tr.expire()
	if code := get(t, h, "/scrape?info_hash="+hashA, "10.0.0.1:1", nil); code != http.StatusNotFound {
		t.Errorf("swarm with only expired peers survived expire(): code %d", code)
	}
}

func TestTracker_RunStopsOnCancel(t *testing.T) {
	t.Parallel()
	tr := New(time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tr.Run(ctx)
		close(done)
	}()
	time.Sleep(5 * time.Millisecond) // let a few sweeps run
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestTracker_Handler_RoutesAndMethods(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()
	if code := get(t, h, "/healthz", "10.0.0.1:1", nil); code != http.StatusOK {
		t.Errorf("healthz code = %d", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/announce", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /announce code = %d, want 405", rec.Code)
	}
}

func TestTracker_ConcurrentAnnounces(t *testing.T) {
	t.Parallel()
	tr, _ := newTestTracker()
	h := tr.Handler()
	const workers, rounds = 50, 20
	hashes := []string{hashA, hashB}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				announce(t, h, hashes[w%2], w, int64(w%3), "")
				get(t, h, "/scrape?info_hash="+hashes[w%2], "10.0.0.1:1", nil)
			}
		}(w)
	}
	wg.Wait()

	total := 0
	for _, ih := range hashes {
		var stats map[string]int
		get(t, h, "/scrape?info_hash="+ih, "10.0.0.1:1", &stats)
		total += stats["complete"] + stats["incomplete"]
	}
	if total != workers {
		t.Errorf("tracked %d peers after concurrent announces, want %d", total, workers)
	}
}
