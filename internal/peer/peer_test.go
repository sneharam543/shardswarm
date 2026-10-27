// Tests for peer connections over real loopback TCP: handshakes, dialling rules, strikes, timeouts, keep-alives, limits, upload pacing, and clean shutdown.
// Owner: Rishav Sarkar (Download Engine & Concurrency)

package peer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/wire"
)

// testMeta has 3 pieces of one block each, so a bitfield is one byte.
var testMeta = func() *metainfo.MetaInfo {
	m, err := metainfo.Build(bytes.NewReader(make([]byte, 3*wire.BlockSize)), "f.bin", wire.BlockSize)
	if err != nil {
		panic(err)
	}
	return m
}()

const wait = 3 * time.Second

type event struct {
	kind string // "connected", "message" or "disconnected"
	c    *Conn
	m    wire.Message
}

type recorder struct{ ch chan event }

func (r *recorder) Connected(c *Conn)               { r.ch <- event{kind: "connected", c: c} }
func (r *recorder) Message(c *Conn, m wire.Message) { r.ch <- event{kind: "message", c: c, m: m} }
func (r *recorder) Disconnected(c *Conn, err error) { r.ch <- event{kind: "disconnected", c: c} }
func (r *recorder) next(t *testing.T, kind string) event {
	t.Helper()
	select {
	case e := <-r.ch:
		if e.kind != kind {
			t.Fatalf("got %s event (%+v), want %s", e.kind, e.m, kind)
		}
		return e
	case <-time.After(wait):
		t.Fatalf("no %s event within %v", kind, wait)
		return event{}
	}
}

// none asserts that no event arrives for d.
func (r *recorder) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case e := <-r.ch:
		t.Fatalf("unexpected %s event (%+v)", e.kind, e.m)
	case <-time.After(d):
	}
}

type testPeer struct {
	*Manager
	rec  *recorder
	addr string
	stop func() // cancels the Manager and waits for everything to exit
}

func newTestPeer(t *testing.T, id byte, tune func(m *Manager)) *testPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{ch: make(chan event, 100)}
	m := NewManager(ctx, testMeta, [20]byte{id}, rec)
	m.handshakeTimeout = 500 * time.Millisecond
	if tune != nil {
		tune(m)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- m.Serve(ln) }()
	stop := sync.OnceFunc(func() {
		cancel()
		m.Wait()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	t.Cleanup(stop)
	return &testPeer{Manager: m, rec: rec, addr: ln.Addr().String(), stop: stop}
}

// dialRaw opens a plain TCP connection, with no handshake, to act as a
// hand-written peer.
func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return nc
}

// handshakeRaw dials addr and completes a valid handshake as peer id.
func handshakeRaw(t *testing.T, addr string, id byte) net.Conn {
	t.Helper()
	nc := dialRaw(t, addr)
	if err := wire.WriteHandshake(nc, testMeta.InfoHashBytes(), [20]byte{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadHandshake(nc, testMeta.InfoHashBytes(), [20]byte{id}); err != nil {
		t.Fatal(err)
	}
	return nc
}

// expectClosed asserts the remote side closes nc within wait.
func expectClosed(t *testing.T, nc net.Conn) {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(wait))
	_, err := io.Copy(io.Discard, nc)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("connection still open")
	}
}

func TestManager_ConnectAndExchange(t *testing.T) {
	t.Parallel()
	a, b := newTestPeer(t, 1, nil), newTestPeer(t, 2, nil)
	a.Dial(b.addr, [20]byte{2})
	ca, cb := a.rec.next(t, "connected").c, b.rec.next(t, "connected").c
	if ca.ID != [20]byte{2} || cb.ID != [20]byte{1} {
		t.Fatalf("peer IDs: a sees %x, b sees %x", ca.ID, cb.ID)
	}
	if !ca.AmChoking() || !ca.PeerChoking() || ca.AmInterested() || ca.PeerInterested() {
		t.Error("connection did not start choked and not interested")
	}

	cb.Send(wire.Message{ID: wire.Bitfield, Data: []byte{0xE0}})
	if e := a.rec.next(t, "message"); e.m.ID != wire.Bitfield {
		t.Fatalf("got %+v, want bitfield", e.m)
	}

	ca.Send(wire.Message{ID: wire.Interested})
	if e := b.rec.next(t, "message"); e.m.ID != wire.Interested || !cb.PeerInterested() || !ca.AmInterested() {
		t.Fatalf("interest not tracked: %+v", e.m)
	}

	// b still chokes a: the request is dropped, and the Have behind it shows
	// that b has already read past it.
	ca.Send(wire.Message{ID: wire.Request, Index: 0, Begin: 0, Length: wire.BlockSize})
	ca.Send(wire.Message{ID: wire.Have, Index: 1})
	if e := b.rec.next(t, "message"); e.m.ID != wire.Have {
		t.Fatalf("got %+v, want the request dropped and the have delivered", e.m)
	}

	cb.Send(wire.Message{ID: wire.Unchoke})
	if e := a.rec.next(t, "message"); e.m.ID != wire.Unchoke || ca.PeerChoking() || cb.AmChoking() {
		t.Fatalf("unchoke not tracked: %+v", e.m)
	}
	ca.Send(wire.Message{ID: wire.Request, Index: 2, Begin: 0, Length: wire.BlockSize})
	if e := b.rec.next(t, "message"); e.m.ID != wire.Request || e.m.Index != 2 {
		t.Fatalf("got %+v, want request for piece 2", e.m)
	}

	ca.Close()
	a.rec.next(t, "disconnected")
	b.rec.next(t, "disconnected")
}

func TestManager_DialRules(t *testing.T) {
	t.Parallel()
	a, b := newTestPeer(t, 1, nil), newTestPeer(t, 2, nil)
	b.Dial(b.addr, [20]byte{2}) // self
	// a expects ID 9 at b's address but finds ID 2: a refuses the connection.
	a.Dial(b.addr, [20]byte{9})
	a.rec.none(t, 300*time.Millisecond)

	// Either side may dial: here the higher ID does.
	c, d := newTestPeer(t, 3, nil), newTestPeer(t, 4, nil)
	d.Dial(c.addr, [20]byte{3})
	if got := c.rec.next(t, "connected").c.ID; got != ([20]byte{4}) {
		t.Errorf("c connected to %x, want 4", got)
	}
	d.rec.next(t, "connected")
}

// live returns m's only connection once it has exactly one and it has been
// stable for a moment, or fails.
func live(t *testing.T, m *Manager) *Conn {
	t.Helper()
	deadline := time.Now().Add(wait)
	var last *Conn
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		m.mu.Lock()
		var c *Conn
		if len(m.conns) == 1 {
			for _, v := range m.conns {
				c = v
			}
		}
		m.mu.Unlock()
		if c != last {
			last, stableSince = c, time.Now()
		}
		if c != nil && time.Since(stableSince) > 200*time.Millisecond {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no single stable connection")
	return nil
}

// Both peers dial each other at once. Each side must end with exactly one
// connection, the same TCP connection on both sides (the one dialled by
// the lower ID), and it must carry messages.
func TestManager_SimultaneousDialKeepsOneConnection(t *testing.T) {
	t.Parallel()
	a, b := newTestPeer(t, 1, nil), newTestPeer(t, 2, nil)
	a.Dial(b.addr, [20]byte{2})
	b.Dial(a.addr, [20]byte{1})

	ca, cb := live(t, a.Manager), live(t, b.Manager)
	if ca.nc.LocalAddr().String() != cb.nc.RemoteAddr().String() {
		t.Fatalf("sides kept different connections: a %v->%v, b %v->%v",
			ca.nc.LocalAddr(), ca.nc.RemoteAddr(), cb.nc.LocalAddr(), cb.nc.RemoteAddr())
	}
	if !ca.outbound || cb.outbound {
		t.Error("survivor was not the connection dialled by the lower ID")
	}

	ca.Send(wire.Message{ID: wire.Have, Index: 2})
	deadline := time.After(wait)
	for {
		select {
		case e := <-b.rec.ch:
			if e.kind == "message" && e.c == cb && e.m.ID == wire.Have && e.m.Index == 2 {
				return
			}
		case <-deadline:
			t.Fatal("message did not arrive over the surviving connection")
		}
	}
}

func TestManager_RejectsBadHandshakes(t *testing.T) {
	t.Parallel()
	a := newTestPeer(t, 1, nil)

	var otherSwarm [32]byte
	nc := dialRaw(t, a.addr)
	_ = wire.WriteHandshake(nc, otherSwarm, [20]byte{5})
	expectClosed(t, nc)

	nc = dialRaw(t, a.addr)
	_, _ = nc.Write(bytes.Repeat([]byte("junk"), 16))
	expectClosed(t, nc)

	nc = dialRaw(t, a.addr) // says nothing: closed by the handshake timeout
	expectClosed(t, nc)

	a.rec.none(t, 100*time.Millisecond)
}

func TestConn_StrikesDisconnect(t *testing.T) {
	t.Parallel()
	a := newTestPeer(t, 1, nil)
	nc := handshakeRaw(t, a.addr, 5)
	a.rec.next(t, "connected")

	unknownID := []byte{0, 0, 0, 1, 99}
	var frames bytes.Buffer
	frames.Write(unknownID) // strike 1
	_ = wire.Write(&frames, wire.Message{ID: wire.Have, Index: 0})
	_ = wire.Write(&frames, wire.Message{ID: wire.Bitfield, Data: []byte{0xE0}}) // not first: strike 2
	frames.Write(unknownID)                                                      // strike 3
	if _, err := nc.Write(frames.Bytes()); err != nil {
		t.Fatal(err)
	}

	if e := a.rec.next(t, "message"); e.m.ID != wire.Have {
		t.Fatalf("got %+v, want the valid have delivered", e.m)
	}
	a.rec.next(t, "disconnected")
	expectClosed(t, nc)
}

func TestConn_IdleTimeout(t *testing.T) {
	t.Parallel()
	a := newTestPeer(t, 1, func(m *Manager) { m.idleTimeout = 200 * time.Millisecond })
	nc := handshakeRaw(t, a.addr, 5)
	a.rec.next(t, "connected")
	a.rec.next(t, "disconnected") // nc never sends anything
	expectClosed(t, nc)
}

func TestConn_KeepAliveHoldsIdleLink(t *testing.T) {
	t.Parallel()
	tune := func(m *Manager) {
		m.idleTimeout = 300 * time.Millisecond
		m.keepAliveEvery = 50 * time.Millisecond
	}
	a, b := newTestPeer(t, 1, tune), newTestPeer(t, 2, tune)
	a.Dial(b.addr, [20]byte{2})
	a.rec.next(t, "connected")
	b.rec.next(t, "connected")
	a.rec.none(t, time.Second) // three idle timeouts pass with no disconnect
	b.rec.none(t, 0)
}

func TestManager_ConnLimitAndDuplicates(t *testing.T) {
	t.Parallel()
	a := newTestPeer(t, 1, func(m *Manager) { m.maxConns = 2 })
	handshakeRaw(t, a.addr, 5)
	a.rec.next(t, "connected")

	dup := dialRaw(t, a.addr) // same peer ID as the live connection
	_ = wire.WriteHandshake(dup, testMeta.InfoHashBytes(), [20]byte{5})
	expectClosed(t, dup)

	handshakeRaw(t, a.addr, 6)
	a.rec.next(t, "connected")
	over := dialRaw(t, a.addr) // third connection: over the limit
	_ = wire.WriteHandshake(over, testMeta.InfoHashBytes(), [20]byte{7})
	expectClosed(t, over)
	a.rec.none(t, 100*time.Millisecond)
}

func TestConn_SlowPeerDropped(t *testing.T) {
	t.Parallel()
	a := newTestPeer(t, 1, func(m *Manager) { m.sendQueue = 4 })
	handshakeRaw(t, a.addr, 5) // never reads, so a's writes back up
	c := a.rec.next(t, "connected").c

	block := wire.Message{ID: wire.Piece, Data: make([]byte, wire.BlockSize)}
	for i := 0; i < 100_000; i++ {
		select {
		case <-c.done:
			a.rec.next(t, "disconnected")
			return
		default:
			c.Send(block)
		}
	}
	t.Fatal("connection to a peer that never reads was not dropped")
}

// A low upload limit paces writes far beyond the write timeout; each chunk
// gets a fresh deadline, so the link must survive and the data must arrive
// no faster than the limit allows.
func TestConn_UploadLimitPacesWrites(t *testing.T) {
	t.Parallel()
	a := newTestPeer(t, 1, func(m *Manager) { m.writeTimeout = 100 * time.Millisecond })
	b := newTestPeer(t, 2, nil)
	a.LimitUpload(32 << 10) // 32 KiB/s; the bucket starts with 16 KiB
	a.Dial(b.addr, [20]byte{2})
	ca := a.rec.next(t, "connected").c
	b.rec.next(t, "connected")

	start := time.Now()
	for i := uint32(0); i < 3; i++ { // 48 KiB of blocks
		ca.Send(wire.Message{ID: wire.Piece, Index: i, Data: make([]byte, wire.BlockSize)})
	}
	for i := 0; i < 3; i++ {
		if e := b.rec.next(t, "message"); e.m.ID != wire.Piece {
			t.Fatalf("got %+v, want a piece", e.m)
		}
	}
	// 48 KiB minus the 16 KiB bucket at 32 KiB/s is about 1 s.
	if d := time.Since(start); d < 800*time.Millisecond || d > 2*time.Second {
		t.Errorf("3 blocks took %v at 32 KiB/s, want about 1s", d)
	}
	a.rec.none(t, 100*time.Millisecond) // no disconnect from the short write timeout
}

// Control messages skip the upload limiter: a Have to one peer must not wait
// behind a backlog of capped Piece data queued for another peer.
func TestConn_ControlMessagesBypassUploadLimit(t *testing.T) {
	t.Parallel()
	a, b, c := newTestPeer(t, 1, nil), newTestPeer(t, 2, nil), newTestPeer(t, 3, nil)
	a.LimitUpload(16 << 10) // 16 KiB/s: three blocks are about 2 s of backlog
	a.Dial(b.addr, [20]byte{2})
	a.Dial(c.addr, [20]byte{3})
	conns := map[byte]*Conn{}
	for range 2 {
		e := a.rec.next(t, "connected")
		conns[e.c.ID[0]] = e.c
	}
	b.rec.next(t, "connected")
	c.rec.next(t, "connected")

	for i := uint32(0); i < 3; i++ {
		conns[2].Send(wire.Message{ID: wire.Piece, Index: i, Data: make([]byte, wire.BlockSize)})
	}
	time.Sleep(50 * time.Millisecond) // the piece writer has reserved the limiter
	start := time.Now()
	conns[3].Send(wire.Message{ID: wire.Have, Index: 1})
	if e := c.rec.next(t, "message"); e.m.ID != wire.Have {
		t.Fatalf("got %+v, want have", e.m)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("Have took %v behind capped piece data, want it unthrottled", d)
	}
}

// Not parallel: it compares goroutine counts.
func TestManager_ShutdownClosesEverything(t *testing.T) {
	before := runtime.NumGoroutine()
	a := newTestPeer(t, 1, func(m *Manager) { m.handshakeTimeout = time.Minute })
	b := newTestPeer(t, 2, nil)
	a.Dial(b.addr, [20]byte{2})
	a.rec.next(t, "connected")
	b.rec.next(t, "connected")
	pending := dialRaw(t, a.addr) // stuck mid-handshake

	start := time.Now()
	a.stop()
	if d := time.Since(start); d > wait {
		t.Errorf("shutdown took %v", d)
	}
	a.rec.next(t, "disconnected")
	b.rec.next(t, "disconnected")
	expectClosed(t, pending)
	b.stop()

	deadline := time.Now().Add(wait)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("%d goroutines after shutdown, %d before: leak", n, before)
	}
}
