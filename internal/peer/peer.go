// Owner: Rishav Sarkar (Download Engine & Concurrency)

// Package peer manages connections to other peers: handshakes, per-connection
// read and write loops, choke/interest state, timeouts and strikes. What to
// do with each message is up to a Handler (the download engine).
package peer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/ratelimit"
	"github.com/sneharam543/shardswarm/internal/wire"
)

const maxStrikes = 3

// Handler receives connection events. Calls for one connection come from
// that connection's read goroutine, one at a time; calls for different
// connections run concurrently, so a Handler must lock its own state.
// A Handler must not block for long: while it runs, that peer is not read.
type Handler interface {
	Connected(c *Conn)
	// Message receives every valid message except KeepAlive. Choke and
	// interest messages arrive after the Conn's flags are updated. Requests
	// that arrive while we choke the peer are dropped before this.
	Message(c *Conn, m wire.Message)
	Disconnected(c *Conn, err error)
}

// Manager owns all connections for one swarm.
type Manager struct {
	meta    *metainfo.MetaInfo
	self    [20]byte
	handler Handler
	ctx     context.Context

	// Limits and timeouts (docs/PROTOCOL.md §6); tests shrink them.
	maxConns         int
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
	keepAliveEvery   time.Duration
	writeTimeout     time.Duration
	sendQueue        int

	wg      sync.WaitGroup
	mu      sync.Mutex
	conns   map[[20]byte]*Conn
	waiting bool // set by Wait; no new goroutines may start after it

	upload atomic.Pointer[ratelimit.Limiter] // nil: unlimited
}

// NewManager returns a Manager for meta's swarm. Cancelling ctx closes the
// listener and every connection; call Wait to block until they are gone.
func NewManager(ctx context.Context, meta *metainfo.MetaInfo, self [20]byte, h Handler) *Manager {
	return &Manager{
		meta:             meta,
		self:             self,
		handler:          h,
		ctx:              ctx,
		maxConns:         50,
		dialTimeout:      5 * time.Second,
		handshakeTimeout: 10 * time.Second,
		idleTimeout:      120 * time.Second,
		keepAliveEvery:   60 * time.Second,
		writeTimeout:     30 * time.Second,
		sendQueue:        256,
		conns:            make(map[[20]byte]*Conn),
	}
}

// Serve accepts connections on ln until the Manager's context is cancelled.
func (m *Manager) Serve(ln net.Listener) error {
	stop := context.AfterFunc(m.ctx, func() { ln.Close() })
	defer stop()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if m.ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !m.spawn(func() { m.handle(nc, nil) }) {
			nc.Close()
		}
	}
}

// Dial connects to peer id at addr in the background, unless id is us or
// already connected. Both peers of a pair may dial at once; add keeps the
// same one of the two connections on both sides.
func (m *Manager) Dial(addr string, id [20]byte) {
	if id == m.self || m.connected(id) {
		return
	}
	m.spawn(func() {
		d := net.Dialer{Timeout: m.dialTimeout}
		nc, err := d.DialContext(m.ctx, "tcp", addr)
		if err != nil {
			slog.Debug("dial failed", "addr", addr, "err", err)
			return
		}
		m.handle(nc, &id)
	})
}

// LimitUpload caps the bytes per second this peer sends, shared by all of
// its connections. 0 removes the cap. Safe to call at any time.
func (m *Manager) LimitUpload(bytesPerSec int64) {
	if bytesPerSec <= 0 {
		m.upload.Store(nil)
		return
	}
	m.upload.Store(ratelimit.New(bytesPerSec))
}

// Wait blocks until every connection goroutine has exited. Call it after
// cancelling the Manager's context; no new connections start once it runs.
func (m *Manager) Wait() {
	m.mu.Lock()
	m.waiting = true
	m.mu.Unlock()
	m.wg.Wait()
}

// spawn runs f in a goroutine that Wait waits for. WaitGroup.Add must not
// race with Wait, so both go through m.mu, and nothing starts once Wait has.
func (m *Manager) spawn(f func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting {
		return false
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		f()
	}()
	return true
}

// handle runs one connection from handshake to close. want is the peer ID we
// dialled, or nil for an incoming connection.
func (m *Manager) handle(nc net.Conn, want *[20]byte) {
	defer nc.Close()
	// Cancelling the Manager closes the socket at any stage, which unblocks
	// the handshake or the read loop; everything else unwinds from there.
	stop := context.AfterFunc(m.ctx, func() { nc.Close() })
	defer stop()
	// ponytail: unauthenticated sockets are bounded only by the handshake
	// timeout; add a pending-handshake semaphore if peers face the internet.
	if m.full() {
		return
	}
	infoHash := m.meta.InfoHashBytes()
	_ = nc.SetDeadline(time.Now().Add(m.handshakeTimeout))
	if err := wire.WriteHandshake(nc, infoHash, m.self); err != nil {
		return
	}
	remote, err := wire.ReadHandshake(nc, infoHash, m.self)
	if err != nil || (want != nil && remote != *want) {
		slog.Debug("handshake rejected", "addr", nc.RemoteAddr(), "err", err)
		return
	}
	_ = nc.SetDeadline(time.Time{})

	c := &Conn{
		ID:       remote,
		outbound: want != nil,
		nc:       nc,
		m:        m,
		out:      make(chan wire.Message, m.sendQueue),
		done:     make(chan struct{}),
	}
	c.amChoking.Store(true)
	c.peerChoking.Store(true)
	if !m.add(c) {
		return
	}
	defer m.remove(c)
	c.run()
}

func (m *Manager) full() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns) >= m.maxConns
}

func (m *Manager) connected(id [20]byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[id] != nil
}

// add registers c, keeping at most one connection per peer. When two exist
// (both peers dialled at once), the one dialled by the lower peer ID wins.
// Both sides apply the same rule, so they keep the same TCP connection.
func (m *Manager) add(c *Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.conns[c.ID]
	switch {
	case old == nil && len(m.conns) >= m.maxConns:
		return false
	case old != nil && (m.preferred(old) || !m.preferred(c)):
		return false
	case old != nil:
		old.Close() // its cleanup sees it was replaced and leaves c alone
	}
	m.conns[c.ID] = c
	return true
}

// preferred reports whether c was dialled by the lower ID of the pair.
func (m *Manager) preferred(c *Conn) bool {
	weAreLower := bytes.Compare(m.self[:], c.ID[:]) < 0
	return c.outbound == weAreLower
}

func (m *Manager) remove(c *Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[c.ID] == c {
		delete(m.conns, c.ID)
	}
}

// Conn is one live, handshaken connection. Its methods are safe for
// concurrent use.
type Conn struct {
	ID       [20]byte
	outbound bool // we dialled it

	nc        net.Conn
	m         *Manager
	out       chan wire.Message
	done      chan struct{}
	closeOnce sync.Once
	strikes   atomic.Int32

	amChoking, amInterested, peerChoking, peerInterested atomic.Bool
}

// AmChoking reports whether we refuse the peer's requests.
func (c *Conn) AmChoking() bool { return c.amChoking.Load() }

// AmInterested reports whether we told the peer we want its pieces.
func (c *Conn) AmInterested() bool { return c.amInterested.Load() }

// PeerChoking reports whether the peer refuses our requests.
func (c *Conn) PeerChoking() bool { return c.peerChoking.Load() }

// PeerInterested reports whether the peer wants our pieces.
func (c *Conn) PeerInterested() bool { return c.peerInterested.Load() }

// Send queues m without blocking and updates our choke/interest flags. If
// the queue is full the peer is not keeping up, and the connection closes.
func (c *Conn) Send(m wire.Message) {
	switch m.ID {
	case wire.Choke, wire.Unchoke:
		c.amChoking.Store(m.ID == wire.Choke)
	case wire.Interested, wire.NotInterested:
		c.amInterested.Store(m.ID == wire.Interested)
	}
	select {
	case c.out <- m:
	default:
		slog.Debug("send queue full, dropping peer", "peer", shortID(c.ID))
		c.Close()
	}
}

// Strike records a protocol violation; the third one closes the connection.
func (c *Conn) Strike(reason string) {
	slog.Debug("strike", "peer", shortID(c.ID), "reason", reason)
	if c.strikes.Add(1) >= maxStrikes {
		c.Close()
	}
}

// Close closes the connection. It is safe to call more than once.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

func (c *Conn) run() {
	c.m.handler.Connected(c)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop()
	}()
	err := c.readLoop()
	c.Close()
	<-writerDone
	c.m.handler.Disconnected(c, err)
}

func (c *Conn) readLoop() error {
	first := true
	for {
		_ = c.nc.SetReadDeadline(time.Now().Add(c.m.idleTimeout))
		msg, err := wire.Read(c.nc, c.m.meta)
		if err != nil && !errors.Is(err, wire.ErrInvalid) {
			return err
		}
		wasFirst := first
		if msg.ID != wire.KeepAlive {
			first = false
		}
		if err != nil {
			c.Strike(err.Error())
			continue
		}

		switch msg.ID {
		case wire.KeepAlive:
			continue
		case wire.Choke, wire.Unchoke:
			c.peerChoking.Store(msg.ID == wire.Choke)
		case wire.Interested, wire.NotInterested:
			c.peerInterested.Store(msg.ID == wire.Interested)
		case wire.Bitfield:
			if !wasFirst {
				c.Strike("bitfield after first message")
				continue
			}
		case wire.Request:
			// It may have been sent before our Choke arrived: drop, no strike.
			if c.amChoking.Load() {
				continue
			}
		}
		c.m.handler.Message(c, msg)
	}
}

func (c *Conn) writeLoop() {
	keepAlive := time.NewTicker(c.m.keepAliveEvery)
	defer keepAlive.Stop()
	for {
		var msg wire.Message
		select {
		case <-c.done:
			return
		case msg = <-c.out:
		case <-keepAlive.C:
			msg = wire.Message{ID: wire.KeepAlive}
		}
		if err := wire.Write(pacedWriter{c, msg.ID == wire.Piece}, msg); err != nil {
			c.Close()
			return
		}
	}
}

// pacedWriter writes to the socket, in chunks the upload limiter allows when
// limited is set. Only Piece frames (file data) are limited: requests, Haves
// and choke messages are a few bytes each, and queueing them behind our own
// uploads would slow our downloads. Each chunk gets a fresh write deadline,
// so a low limit is not mistaken for a stalled peer.
type pacedWriter struct {
	c       *Conn
	limited bool
}

func (w pacedWriter) Write(p []byte) (int, error) {
	c, lim := w.c, w.c.m.upload.Load()
	if !w.limited {
		lim = nil
	}
	written := 0
	for written < len(p) {
		chunk := len(p) - written
		if lim != nil {
			chunk = min(chunk, lim.Burst())
			if err := lim.WaitN(c.m.ctx, chunk); err != nil {
				return written, err
			}
		}
		_ = c.nc.SetWriteDeadline(time.Now().Add(c.m.writeTimeout))
		n, err := c.nc.Write(p[written : written+chunk])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// shortID is enough of a peer ID to tell peers apart in logs.
func shortID(id [20]byte) string { return hex.EncodeToString(id[:4]) }
