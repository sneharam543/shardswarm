// Tests for choking: slot selection and tie-breaking, plus tit-for-tat, choke-off and free-rider behaviour against real peers.
// Owner: Swayam Kanjilal (Choking & Experiments)

package piece

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/wire"
)

func TestParseChokePolicy(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"tft", "off", "freerider"} {
		if got, err := ParseChokePolicy(s); err != nil || string(got) != s {
			t.Errorf("ParseChokePolicy(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := ParseChokePolicy("greedy"); err == nil {
		t.Error("accepted an unknown policy")
	}
}

func TestPickUnchoked(t *testing.T) {
	t.Parallel()
	rates := map[string]score{"a": {100, 0}, "b": {90, 0}, "c": {80, 0}, "d": {10, 0}, "e": {0, 0}}
	regular := func(set map[string]bool) bool { return set["a"] && set["b"] && set["c"] }

	t.Run("top three plus one optimistic", func(t *testing.T) {
		set, opt := pickUnchoked(rates, "", false, 3)
		if len(set) != 4 || !regular(set) || (opt != "d" && opt != "e") || !set[opt] {
			t.Errorf("set %v, optimistic %q", set, opt)
		}
	})
	t.Run("keeps optimistic between rotations", func(t *testing.T) {
		for range 50 {
			if _, opt := pickUnchoked(rates, "e", false, 3); opt != "e" {
				t.Fatalf("optimistic changed to %q without rotation", opt)
			}
		}
	})
	t.Run("rotation reaches every choked peer", func(t *testing.T) {
		seen := map[string]bool{}
		for range 200 {
			_, opt := pickUnchoked(rates, "e", true, 3)
			seen[opt] = true
		}
		if len(seen) != 2 || !seen["d"] || !seen["e"] {
			t.Errorf("rotation picked %v, want d and e", seen)
		}
	})
	t.Run("replaces optimistic that earned a regular slot", func(t *testing.T) {
		if set, opt := pickUnchoked(rates, "a", false, 3); opt == "a" || len(set) != 4 {
			t.Errorf("set %v, optimistic %q", set, opt)
		}
	})
	t.Run("replaces optimistic that lost interest", func(t *testing.T) {
		if _, opt := pickUnchoked(rates, "gone", false, 3); opt != "d" && opt != "e" {
			t.Errorf("optimistic %q", opt)
		}
	})
	t.Run("few peers: all unchoked, no optimistic", func(t *testing.T) {
		set, opt := pickUnchoked(map[string]score{"a": {1, 0}, "b": {}}, "", true, 3)
		if len(set) != 2 || opt != "" {
			t.Errorf("set %v, optimistic %q", set, opt)
		}
	})
	t.Run("ties are broken at random", func(t *testing.T) {
		flat := map[string]score{"a": {}, "b": {}, "c": {}, "d": {}, "e": {}}
		seen := map[string]bool{}
		for range 200 {
			set, opt := pickUnchoked(flat, "", false, 3)
			for k := range set {
				if k != opt {
					seen[k] = true
				}
			}
		}
		if len(seen) != 5 {
			t.Errorf("only %v ever got a regular slot", seen)
		}
	})
}

// A quiet round must not put a contributor level with a peer that never
// gave anything: lifetime bytes break the tie, so a free-rider ("fr") only
// ever gets the optimistic slot.
func TestPickUnchoked_HistoryBreaksTies(t *testing.T) {
	t.Parallel()
	quiet := map[string]score{"c1": {0, 900}, "c2": {0, 500}, "c3": {0, 100}, "fr": {0, 0}}
	for range 100 {
		set, opt := pickUnchoked(quiet, "", false, 3)
		if !set["c1"] || !set["c2"] || !set["c3"] || opt != "fr" {
			t.Fatalf("set %v, optimistic %q: contributors must hold the regular slots", set, opt)
		}
	}
}

// Free slots go to the peers that have given us the most.
func TestRanked_ContributorsFirst(t *testing.T) {
	t.Parallel()
	for range 100 {
		got := ranked(map[string]score{"fr": {}, "c1": {total: 5}, "c2": {total: 50}})
		if got[0] != "c2" || got[1] != "c1" || got[2] != "fr" {
			t.Fatalf("order %v, want c2 c1 fr", got)
		}
	}
}

// chokeWatch tracks which raw test peers are unchoked by the peer under
// test, as seen from their side.
type chokeWatch struct {
	mu   sync.Mutex
	now  map[byte]bool // currently unchoked
	ever map[byte]bool // unchoked at least once
}

// interestedPeer connects to target as raw peer id, says Interested, and
// records Choke/Unchoke messages in w. It never requests or uploads.
func interestedPeer(t *testing.T, target string, meta *metainfo.MetaInfo, id byte, w *chokeWatch) net.Conn {
	t.Helper()
	nc, err := net.Dial("tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	self := [20]byte{id}
	_ = wire.WriteHandshake(nc, meta.InfoHashBytes(), self)
	if _, err := wire.ReadHandshake(nc, meta.InfoHashBytes(), self); err != nil {
		t.Fatal(err)
	}
	_ = wire.Write(nc, wire.Message{ID: wire.Interested})
	go func() {
		for {
			m, err := wire.Read(nc, meta)
			if err != nil {
				return
			}
			if m.ID != wire.Choke && m.ID != wire.Unchoke {
				continue
			}
			w.mu.Lock()
			w.now[id] = m.ID == wire.Unchoke
			if w.now[id] {
				w.ever[id] = true
			}
			w.mu.Unlock()
		}
	}()
	return nc
}

// unchokedCount counts interested peers the seeder has unchoked: the ones
// that use a slot. It is the seeder's own view. Raw peers can see a Choke and an
// Unchoke from one rechoke in either order, so only this view is exact.
func (e *Engine) unchokedCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for c := range e.peers {
		if !c.AmChoking() && c.PeerInterested() {
			n++
		}
	}
	return n
}

func watchSeeder(t *testing.T, policy ChokePolicy, peers int, rechoke time.Duration) (*Engine, *chokeWatch, map[byte]net.Conn) {
	t.Helper()
	data, meta := testFile(t)
	seeder := startPeer(t, 1, meta, data, RarestFirst, func(e *Engine) {
		e.cfg.Choke = policy
		e.rechokeEvery = rechoke
	})
	w := &chokeWatch{now: map[byte]bool{}, ever: map[byte]bool{}}
	conns := map[byte]net.Conn{}
	for i := 0; i < peers; i++ {
		id := byte(0xA0 + i)
		conns[id] = interestedPeer(t, seeder.addr, meta, id, w)
	}
	return seeder.e, w, conns
}

// waitEver waits until n distinct raw peers have been unchoked.
func waitEver(t *testing.T, w *chokeWatch, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		w.mu.Lock()
		got := len(w.ever)
		w.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d peers unchoked within %v", got, n, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEngine_TitForTatLimitsAndRotatesSlots(t *testing.T) {
	t.Parallel()
	seeder, w, _ := watchSeeder(t, TitForTat, 7, 50*time.Millisecond)
	deadline := time.Now().Add(wait)
	for {
		if n := seeder.unchokedCount(); n > regularSlots+1 {
			t.Fatalf("%d peers unchoked at once, limit is %d", n, regularSlots+1)
		}
		w.mu.Lock()
		ever := len(w.ever)
		w.mu.Unlock()
		if ever == 7 {
			return // every peer got a turn
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 7 peers were ever unchoked", ever)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEngine_ChokeOffUnchokesEveryone(t *testing.T) {
	t.Parallel()
	_, w, _ := watchSeeder(t, ChokeOff, 7, 50*time.Millisecond)
	deadline := time.Now().Add(wait)
	for {
		w.mu.Lock()
		n := len(w.ever)
		w.mu.Unlock()
		if n == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 7 peers unchoked with choking off", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // several rechoke rounds: nobody is choked again
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, u := range w.now {
		if !u {
			t.Errorf("peer %x choked again with choking off", id)
		}
	}
}

// A slot freed by a peer that lost interest goes to a waiting peer at once,
// not at the next rechoke (set to a minute here, so it never runs).
func TestEngine_FreedSlotGoesToWaitingPeer(t *testing.T) {
	t.Parallel()
	seeder, w, conns := watchSeeder(t, TitForTat, regularSlots+2, time.Minute)
	waitEver(t, w, regularSlots+1, wait) // all slots full, one peer waiting
	time.Sleep(100 * time.Millisecond)   // the waiting peer stays choked
	if n := seeder.unchokedCount(); n != regularSlots+1 {
		t.Fatalf("%d slots in use, want %d", n, regularSlots+1)
	}

	w.mu.Lock()
	var leaver byte
	for id, unchoked := range w.now {
		if unchoked {
			leaver = id
			break
		}
	}
	w.mu.Unlock()
	_ = wire.Write(conns[leaver], wire.Message{ID: wire.NotInterested})
	waitEver(t, w, regularSlots+2, 2*time.Second)
}

// A free-rider downloads (through other peers' unchokes) but never uploads
// a byte and never unchokes anyone, even after it has the whole file.
func TestEngine_FreeRiderNeverUploads(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	tune := func(p ChokePolicy) func(*Engine) {
		return func(e *Engine) { e.cfg.Choke = p; e.rechokeEvery = 50 * time.Millisecond }
	}
	peers := []*swarmPeer{
		startPeer(t, 1, meta, data, RarestFirst, tune(TitForTat)),
		startPeer(t, 2, meta, nil, RarestFirst, tune(TitForTat)),
		startPeer(t, 3, meta, nil, RarestFirst, tune(TitForTat)),
		startPeer(t, 4, meta, nil, RarestFirst, tune(FreeRider)),
	}
	connectAll(peers)
	for _, p := range peers[1:] {
		waitDone(t, p, data, meta.Info.Name)
	}
	freeRider := peers[3]
	if up := freeRider.e.Stats().Uploaded; up != 0 {
		t.Errorf("free-rider uploaded %d bytes", up)
	}

	w := &chokeWatch{now: map[byte]bool{}, ever: map[byte]bool{}}
	interestedPeer(t, freeRider.addr, meta, 0xA0, w)
	time.Sleep(300 * time.Millisecond) // several rechoke rounds
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.ever) != 0 {
		t.Error("free-rider unchoked an interested peer")
	}
}
