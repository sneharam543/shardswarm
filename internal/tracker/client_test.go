// Tests for the tracker client: against a real tracker, and against malformed or hostile responses.
// Owner: Sneha Ram (Metainfo & Tracker)

package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnnounce_AgainstRealTracker(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(New(interval).Handler())
	defer srv.Close()
	ctx := context.Background()
	p := AnnounceParams{InfoHash: [32]byte{1}, PeerID: [20]byte{1}, Port: 7001, Left: 0, Event: "started"}

	iv, peers, err := Announce(ctx, srv.URL, p)
	if err != nil || iv != interval || len(peers) != 0 {
		t.Fatalf("first announce: %v, %v, %v", iv, peers, err)
	}

	p.PeerID, p.Port, p.Left = [20]byte{2}, 7002, 100
	_, peers, err = Announce(ctx, srv.URL, p)
	if err != nil || len(peers) != 1 || peers[0].ID != ([20]byte{1}) || peers[0].Addr != "127.0.0.1:7001" {
		t.Fatalf("second announce: %+v, %v", peers, err)
	}

	p.Port = 0 // rejected by the tracker: the error message must come through
	if _, _, err := Announce(ctx, srv.URL, p); err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want the tracker's port error", err)
	}
}

func TestAnnounce_DistrustsResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		wantErr   bool
		wantPeers int
	}{
		{"malformed JSON", `{"interval":`, true, 0},
		{"zero interval", `{"interval":0,"peers":[]}`, true, 0},
		{"bad entries skipped", `{"interval":5,"peers":[
			{"peer_id":"` + strings.Repeat("a", 40) + `","ip":"10.0.0.1","port":1},
			{"peer_id":"short","ip":"10.0.0.2","port":1},
			{"peer_id":"` + strings.Repeat("b", 40) + `","ip":"not-an-ip","port":1},
			{"peer_id":"` + strings.Repeat("c", 40) + `","ip":"10.0.0.3","port":70000},
			{"peer_id":"` + strings.Repeat("d", 40) + `","ip":"::1","port":9}]}`, false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			iv, peers, err := Announce(context.Background(), srv.URL, AnnounceParams{Port: 1})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(peers) != tt.wantPeers {
				t.Errorf("got %d peers, want %d: %+v", len(peers), tt.wantPeers, peers)
			}
			if !tt.wantErr && iv != 5*time.Second {
				t.Errorf("interval = %v", iv)
			}
			if tt.wantPeers == 2 && peers[1].Addr != "[::1]:9" {
				t.Errorf("IPv6 address = %q, want [::1]:9", peers[1].Addr)
			}
		})
	}
}
