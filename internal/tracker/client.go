// Tracker client: one announce request, with every peer in the response checked before use.
// Owner: Sneha Ram (Metainfo & Tracker)

package tracker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AnnounceParams is what a peer reports on each announce.
type AnnounceParams struct {
	InfoHash [32]byte
	PeerID   [20]byte
	Port     int
	Left     int64
	Event    string // "started", "completed", "stopped" or ""
}

// Contact is a peer from an announce response, ready to dial.
type Contact struct {
	ID   [20]byte
	Addr string // host:port
}

// Announce calls GET /announce on the tracker at base (for example
// "http://tracker:8080") and returns the re-announce interval and the other
// peers. Malformed peer entries are skipped: the response is not trusted.
func Announce(ctx context.Context, base string, p AnnounceParams) (time.Duration, []Contact, error) {
	q := url.Values{
		"info_hash": {hex.EncodeToString(p.InfoHash[:])},
		"peer_id":   {hex.EncodeToString(p.PeerID[:])},
		"port":      {strconv.Itoa(p.Port)},
		"left":      {strconv.FormatInt(p.Left, 10)},
	}
	if p.Event != "" {
		q.Set("event", p.Event)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/announce?"+q.Encode(), nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Interval int    `json:"interval"`
		Peers    []Peer `json:"peers"`
		Error    string `json:"error"`
	}
	// 50 peers is a few KiB; 1 MiB bounds a hostile or broken tracker.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return 0, nil, fmt.Errorf("announce: %s: decode response: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("announce: %s: %s", resp.Status, body.Error)
	}
	if body.Interval < 1 {
		return 0, nil, fmt.Errorf("announce: invalid interval %d", body.Interval)
	}

	contacts := make([]Contact, 0, len(body.Peers))
	for _, peer := range body.Peers {
		var c Contact
		if !isLowerHex(peer.PeerID, 40) || peer.Port < 1 || peer.Port > 65535 || net.ParseIP(peer.IP) == nil {
			continue
		}
		hex.Decode(c.ID[:], []byte(peer.PeerID))
		c.Addr = net.JoinHostPort(peer.IP, strconv.Itoa(peer.Port))
		contacts = append(contacts, c)
	}
	return time.Duration(body.Interval) * time.Second, contacts, nil
}
