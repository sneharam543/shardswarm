// Tests for the engine: piece picking, real swarms over TCP for each strategy, corrupt, silent and nosy peers, upload limits, and re-seeding.
// Owner: Rishav Sarkar (Download Engine & Concurrency)

package piece

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/peer"
	"github.com/sneharam543/shardswarm/internal/wire"
)

const wait = 20 * time.Second

func TestParseStrategy(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"rarest", "random", "sequential"} {
		if got, err := ParseStrategy(s); err != nil || string(got) != s {
			t.Errorf("ParseStrategy(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := ParseStrategy("fastest"); err == nil {
		t.Error("ParseStrategy accepted an unknown strategy")
	}
}

func TestEngine_PickPiece(t *testing.T) {
	t.Parallel()
	// Six pieces. We have piece 1, piece 3 is in progress, and the peer
	// lacks piece 4, so the candidates are 0, 2 and 5. Pieces 2 and 5 are
	// the rarest (two peers each).
	newEngine := func(s Strategy) *Engine {
		e := &Engine{cfg: Config{Strategy: s}, have: NewBitfield(6), avail: []int{3, 1, 2, 1, 0, 2},
			active: map[int]*download{3: {}}}
		e.have.Set(1)
		return e
	}
	peerHas := NewBitfield(6)
	for _, i := range []int{0, 1, 2, 3, 5} {
		peerHas.Set(i)
	}
	picks := func(s Strategy) map[int]int {
		e, seen := newEngine(s), map[int]int{}
		for range 500 {
			i, ok := e.pickPiece(peerHas)
			if !ok {
				t.Fatalf("%s: no pick", s)
			}
			seen[i]++
		}
		return seen
	}

	if got := picks(Sequential); len(got) != 1 || got[0] != 500 {
		t.Errorf("sequential picked %v, want always 0", got)
	}
	if got := picks(RarestFirst); len(got) != 2 || got[2] == 0 || got[5] == 0 {
		t.Errorf("rarest picked %v, want both 2 and 5 and nothing else", got)
	}
	if got := picks(Random); len(got) != 3 || got[0] == 0 || got[2] == 0 || got[5] == 0 {
		t.Errorf("random picked %v, want 0, 2 and 5", got)
	}
	if _, ok := newEngine(RarestFirst).pickPiece(NewBitfield(6)); ok {
		t.Error("picked a piece from a peer with nothing")
	}
}

type swarmPeer struct {
	m      *peer.Manager
	e      *Engine
	addr   string
	id     [20]byte
	dir    string
	cancel context.CancelFunc
}

// startPeer runs a full peer on loopback. If seed is non-nil the peer
// starts with that content on disk.
func startPeer(t *testing.T, id byte, meta *metainfo.MetaInfo, seed []byte, s Strategy, tune func(*Engine)) *swarmPeer {
	t.Helper()
	dir := t.TempDir()
	if seed != nil {
		if err := os.WriteFile(filepath.Join(dir, meta.Info.Name), seed, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, have, err := OpenStore(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(meta, store, have, Config{Strategy: s, Pipeline: 5, Choke: TitForTat})
	if tune != nil {
		tune(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := peer.NewManager(ctx, meta, [20]byte{id}, e)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go m.Serve(ln)
	go e.Run(ctx)
	t.Cleanup(func() {
		cancel()
		m.Wait()
		store.Close()
	})
	return &swarmPeer{m: m, e: e, addr: ln.Addr().String(), id: [20]byte{id}, dir: dir, cancel: cancel}
}

func connectAll(peers []*swarmPeer) {
	for _, a := range peers {
		for _, b := range peers {
			a.m.Dial(b.addr, b.id) // only the lower ID actually dials
		}
	}
}

func waitDone(t *testing.T, p *swarmPeer, data []byte, name string) {
	t.Helper()
	select {
	case <-p.e.Done():
	case <-time.After(wait):
		t.Fatalf("peer %x did not finish within %v", p.id[0], wait)
	}
	got, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("peer %x: downloaded file differs from the source (err %v)", p.id[0], err)
	}
}

// The upload limit governs real transfer time: a 300 KiB download from a
// seeder capped at 256 KiB/s takes about a second, not milliseconds.
func TestEngine_UploadLimitGovernsDownloadTime(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	seeder := startPeer(t, 1, meta, data, RarestFirst, nil)
	seeder.m.LimitUpload(256 << 10)
	leecher := startPeer(t, 2, meta, nil, RarestFirst, nil)

	start := time.Now()
	connectAll([]*swarmPeer{seeder, leecher})
	waitDone(t, leecher, data, meta.Info.Name)
	// (307200 B - 16 KiB bucket) / 256 KiB/s = 1.11 s, plus protocol overhead.
	if d := time.Since(start); d < 900*time.Millisecond || d > 2*time.Second {
		t.Errorf("download took %v with a 256 KiB/s seeder, want about 1.1s", d)
	}
}

// One seeder and four leechers, as independent peers over TCP.
func TestEngine_SwarmDownload(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	for _, s := range []Strategy{RarestFirst, Random, Sequential} {
		t.Run(string(s), func(t *testing.T) {
			t.Parallel()
			peers := []*swarmPeer{startPeer(t, 1, meta, data, s, nil)}
			for id := byte(2); id <= 5; id++ {
				peers = append(peers, startPeer(t, id, meta, nil, s, nil))
			}
			if l := peers[0].e.Stats().Left; l != 0 {
				t.Errorf("seeder Left = %d, want 0", l)
			}
			if l := peers[1].e.Stats().Left; l != meta.Info.Length {
				t.Errorf("new leecher Left = %d, want %d", l, meta.Info.Length)
			}
			connectAll(peers)
			for _, p := range peers[1:] {
				waitDone(t, p, data, meta.Info.Name)
				if l := p.e.Stats().Left; l != 0 {
					t.Errorf("finished leecher Left = %d, want 0", l)
				}
			}
		})
	}
}

// fakePeer dials target, claims every piece, unchokes it, and answers each
// request according to reply (nil reply: never answer). served counts the
// requests it saw; closed is closed when the target hangs up.
func fakePeer(t *testing.T, target *swarmPeer, meta *metainfo.MetaInfo, reply func(m wire.Message) []byte) (served chan struct{}, closed chan struct{}) {
	t.Helper()
	nc, err := net.Dial("tcp", target.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	self := [20]byte{0xEE}
	if err := wire.WriteHandshake(nc, meta.InfoHashBytes(), self); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadHandshake(nc, meta.InfoHashBytes(), self); err != nil {
		t.Fatal(err)
	}
	all := NewBitfield(meta.NumPieces())
	for i := 0; i < meta.NumPieces(); i++ {
		all.Set(i)
	}
	_ = wire.Write(nc, wire.Message{ID: wire.Bitfield, Data: all})
	_ = wire.Write(nc, wire.Message{ID: wire.Unchoke})

	served, closed = make(chan struct{}, 1000), make(chan struct{})
	go func() {
		defer close(closed)
		for {
			m, err := wire.Read(nc, meta)
			if err != nil {
				return
			}
			if m.ID != wire.Request {
				continue
			}
			served <- struct{}{}
			if reply != nil {
				_ = wire.Write(nc, wire.Message{ID: wire.Piece, Index: m.Index, Begin: m.Begin, Data: reply(m)})
			}
		}
	}()
	return served, closed
}

func waitFor(t *testing.T, ch chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-ch:
		case <-time.After(wait):
			t.Fatalf("timed out waiting for event")
		}
	}
}

// A peer that sends wrong data loses every piece it touches and is cut off
// after three; the download still completes from an honest seeder.
func TestEngine_RejectsCorruptPeer(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	leecher := startPeer(t, 5, meta, nil, RarestFirst, nil)
	_, closed := fakePeer(t, leecher, meta, func(m wire.Message) []byte {
		return bytes.Repeat([]byte{0xBA}, int(m.Length)) // right size, wrong bytes
	})
	select { // only the liar has data, so it is asked until it is struck out
	case <-closed:
	case <-time.After(wait):
		t.Fatal("corrupt peer was not disconnected")
	}

	seeder := startPeer(t, 9, meta, data, RarestFirst, nil)
	connectAll([]*swarmPeer{leecher, seeder})
	waitDone(t, leecher, data, meta.Info.Name)
}

// A peer that never answers holds pieces only until the request timeout.
func TestEngine_ReassignsStalledPieces(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	leecher := startPeer(t, 5, meta, nil, RarestFirst, func(e *Engine) {
		e.requestTimeout = 300 * time.Millisecond
	})
	served, _ := fakePeer(t, leecher, meta, nil)
	waitFor(t, served, 5) // the pipeline is full of requests that never return

	seeder := startPeer(t, 9, meta, data, RarestFirst, nil)
	connectAll([]*swarmPeer{leecher, seeder})
	waitDone(t, leecher, data, meta.Info.Name)
}

// A peer may only fetch pieces we announced: asking a leecher for data it
// does not have (unverified bytes on disk) is a strike, and three cut it off.
func TestEngine_RefusesUnannouncedPieces(t *testing.T) {
	t.Parallel()
	_, meta := testFile(t)
	leecher := startPeer(t, 5, meta, nil, RarestFirst, nil)
	nc, err := net.Dial("tcp", leecher.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	self := [20]byte{0xEE}
	_ = wire.WriteHandshake(nc, meta.InfoHashBytes(), self)
	if _, err := wire.ReadHandshake(nc, meta.InfoHashBytes(), self); err != nil {
		t.Fatal(err)
	}
	_ = wire.Write(nc, wire.Message{ID: wire.Interested})
	for {
		m, err := wire.Read(nc, meta)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID == wire.Unchoke {
			break
		}
	}
	for range 3 {
		_ = wire.Write(nc, wire.Message{ID: wire.Request, Index: 0, Begin: 0, Length: wire.BlockSize})
	}
	_ = nc.SetReadDeadline(time.Now().Add(wait))
	for {
		m, err := wire.Read(nc, meta)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("leecher kept serving a peer that asked for unannounced pieces")
			}
			return // disconnected, as required
		}
		if m.ID == wire.Piece {
			t.Fatal("leecher sent data for a piece it does not have")
		}
	}
}

// After finishing, a leecher seeds: a newcomer can download from it alone.
func TestEngine_LeecherSeedsAfterFinishing(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	seeder := startPeer(t, 1, meta, data, Sequential, nil)
	first := startPeer(t, 2, meta, nil, Sequential, nil)
	connectAll([]*swarmPeer{seeder, first})
	waitDone(t, first, data, meta.Info.Name)
	seeder.cancel()

	late := startPeer(t, 3, meta, nil, Sequential, nil)
	connectAll([]*swarmPeer{first, late})
	waitDone(t, late, data, meta.Info.Name)
}
