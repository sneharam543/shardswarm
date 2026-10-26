// Owner: Sneha Ram (Metainfo & Tracker)

// Package tracker keeps swarm membership so peers can find each other. It
// never handles file content. The HTTP API is documented in docs/TRACKER_API.md.
package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	maxSwarms        = 1000
	maxPeersPerSwarm = 1000
	maxPeersReturned = 50
)

var (
	errConflict = errors.New("peer_id is in use by another live address")
	errFull     = errors.New("tracker is at capacity")
)

// Peer is one entry in an announce response.
type Peer struct {
	PeerID string `json:"peer_id"`
	IP     string `json:"ip"`
	Port   int    `json:"port"`
}

type peerState struct {
	Peer
	left     int64
	lastSeen time.Time
}

// Tracker is safe for concurrent use.
type Tracker struct {
	interval time.Duration
	now      func() time.Time // replaced in tests

	// ponytail: one lock for all swarms; per-swarm locks if a large tracker contends.
	mu     sync.Mutex
	swarms map[string]map[string]*peerState // info_hash -> peer_id -> state
}

// New returns a tracker that asks peers to re-announce every interval and
// forgets a peer after two missed intervals.
func New(interval time.Duration) *Tracker {
	return &Tracker{
		interval: interval,
		now:      time.Now,
		swarms:   make(map[string]map[string]*peerState),
	}
}

// Handler serves /announce, /scrape and /healthz. Other methods on these
// paths get 405 and other paths get 404.
func (t *Tracker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /announce", t.handleAnnounce)
	mux.HandleFunc("GET /scrape", t.handleScrape)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Run removes expired peers and empty swarms every interval until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	tick := time.NewTicker(t.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.expire()
		}
	}
}

type announceRequest struct {
	infoHash string
	peerID   string
	port     int
	left     int64
	event    string
}

func (t *Tracker) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	req, err := parseAnnounce(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot determine client address")
		return
	}

	peers, err := t.announce(req, ip)
	switch {
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errFull):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"interval": int(t.interval / time.Second),
			"peers":    peers,
		})
	}
}

func (t *Tracker) handleScrape(w http.ResponseWriter, r *http.Request) {
	infoHash := r.URL.Query().Get("info_hash")
	if !isLowerHex(infoHash, 64) {
		writeError(w, http.StatusBadRequest, "info_hash must be 64 lowercase hex characters")
		return
	}
	complete, incomplete, ok := t.scrape(infoHash)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown swarm")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"complete": complete, "incomplete": incomplete})
}

func parseAnnounce(q url.Values) (announceRequest, error) {
	req := announceRequest{
		infoHash: q.Get("info_hash"),
		peerID:   q.Get("peer_id"),
		event:    q.Get("event"),
	}
	if !isLowerHex(req.infoHash, 64) {
		return req, errors.New("info_hash must be 64 lowercase hex characters")
	}
	if !isLowerHex(req.peerID, 40) {
		return req, errors.New("peer_id must be 40 lowercase hex characters")
	}
	port, err := strconv.Atoi(q.Get("port"))
	if err != nil || port < 1 || port > 65535 {
		return req, errors.New("port must be an integer in [1, 65535]")
	}
	req.port = port
	left, err := strconv.ParseInt(q.Get("left"), 10, 64)
	if err != nil || left < 0 {
		return req, errors.New("left must be a non-negative integer")
	}
	req.left = left
	switch req.event {
	case "", "started", "completed", "stopped":
	default:
		return req, errors.New("event must be empty, started, completed or stopped")
	}
	return req, nil
}

// announce records the peer and returns up to maxPeersReturned other live
// peers in random order.
func (t *Tracker) announce(req announceRequest, ip string) ([]Peer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	swarm := t.swarms[req.infoHash]
	existing, known := swarm[req.peerID]
	sameAddr := known && existing.IP == ip && existing.Port == req.port

	if req.event == "stopped" {
		// Only the peer's own address may remove it, so a peer_id alone
		// cannot be used to kick someone else out.
		if sameAddr {
			delete(swarm, req.peerID)
			if len(swarm) == 0 {
				delete(t.swarms, req.infoHash)
			}
		}
		return []Peer{}, nil
	}

	if known && !sameAddr && t.alive(existing, now) {
		return nil, errConflict
	}
	if swarm == nil {
		if len(t.swarms) >= maxSwarms {
			return nil, errFull
		}
		swarm = make(map[string]*peerState)
		t.swarms[req.infoHash] = swarm
	}
	if !known && len(swarm) >= maxPeersPerSwarm {
		return nil, errFull
	}
	swarm[req.peerID] = &peerState{
		Peer:     Peer{PeerID: req.peerID, IP: ip, Port: req.port},
		left:     req.left,
		lastSeen: now,
	}

	peers := make([]Peer, 0, min(len(swarm)-1, maxPeersReturned))
	for id, p := range swarm {
		if id != req.peerID && t.alive(p, now) {
			peers = append(peers, p.Peer)
		}
	}
	rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })
	if len(peers) > maxPeersReturned {
		peers = peers[:maxPeersReturned]
	}
	return peers, nil
}

func (t *Tracker) scrape(infoHash string) (complete, incomplete int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	swarm, ok := t.swarms[infoHash]
	now := t.now()
	for _, p := range swarm {
		switch {
		case !t.alive(p, now):
		case p.left == 0:
			complete++
		default:
			incomplete++
		}
	}
	return complete, incomplete, ok
}

func (t *Tracker) expire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for infoHash, swarm := range t.swarms {
		for id, p := range swarm {
			if !t.alive(p, now) {
				delete(swarm, id)
			}
		}
		if len(swarm) == 0 {
			delete(t.swarms, infoHash)
		}
	}
}

func (t *Tracker) alive(p *peerState, now time.Time) bool {
	return now.Sub(p.lastSeen) <= 2*t.interval
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client is gone if this fails
}
