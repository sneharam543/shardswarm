// The download engine: picks pieces, pipelines block requests, verifies and stores pieces, announces them, and serves other peers.
// Owner: Rishav Sarkar (Download Engine & Concurrency)

package piece

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/peer"
	"github.com/sneharam543/shardswarm/internal/wire"
)

// Strategy chooses which missing piece to download next.
type Strategy string

const (
	RarestFirst Strategy = "rarest"
	Random      Strategy = "random"
	Sequential  Strategy = "sequential"
)

// ParseStrategy accepts "rarest", "random" or "sequential".
func ParseStrategy(s string) (Strategy, error) {
	switch st := Strategy(s); st {
	case RarestFirst, Random, Sequential:
		return st, nil
	}
	return "", fmt.Errorf("unknown strategy %q (want rarest, random or sequential)", s)
}

// Engine downloads and serves one file. It implements peer.Handler.
//
// Each piece being downloaded is owned by one peer, which supplies all of
// its blocks. A failed hash check therefore blames exactly one peer, and a
// peer that chokes, leaves or stalls simply loses its pieces back to the pool.
type Engine struct {
	meta           *metainfo.MetaInfo
	store          *Store
	cfg            Config
	requestTimeout time.Duration // a piece with no progress for this long is reassigned
	rechokeEvery   time.Duration

	// ponytail: one lock for all engine state, held during block reads and
	// piece writes; fine for tens of peers, split if profiling says so.
	mu         sync.Mutex
	have       Bitfield
	haveCount  int
	avail      []int // avail[i] = connected peers that have piece i
	peers      map[*peer.Conn]*peerState
	active     map[int]*download // pieces being downloaded, by index
	done       chan struct{}
	uploaded   int64      // total bytes served
	downloaded int64      // total block bytes accepted, including pieces that later failed the hash check
	round      int        // rechoke rounds so far
	optimistic *peer.Conn // current optimistic unchoke, if any
}

// Config selects the engine's behaviour.
type Config struct {
	Strategy Strategy
	Pipeline int // max outstanding block requests per peer
	Choke    ChokePolicy
	// OnPiece, if set, is called after each piece is verified and written.
	// It runs under the engine lock, so it must not call back into the Engine.
	OnPiece func(index int, size int64)
}

type peerState struct {
	has                Bitfield
	inflight           int   // our requests to this peer not yet answered
	roundDown, roundUp int64 // bytes from / to this peer since the last rechoke
	totalDown          int64 // bytes from this peer over the whole connection
}

type download struct {
	index   int
	owner   *peer.Conn
	data    []byte
	got     []bool // per block
	next    int    // next block to request
	missing int    // blocks not yet received
	touched time.Time
}

// NewEngine starts from the pieces store already holds (have).
func NewEngine(meta *metainfo.MetaInfo, store *Store, have Bitfield, cfg Config) *Engine {
	e := &Engine{
		meta:           meta,
		store:          store,
		cfg:            cfg,
		requestTimeout: 30 * time.Second,
		rechokeEvery:   10 * time.Second,
		have:           have,
		avail:          make([]int, meta.NumPieces()),
		peers:          make(map[*peer.Conn]*peerState),
		active:         make(map[int]*download),
		done:           make(chan struct{}),
	}
	for i := 0; i < meta.NumPieces(); i++ {
		if have.Has(i) {
			e.haveCount++
		}
	}
	e.checkComplete()
	return e
}

// Done is closed once every piece is verified and the file has its final name.
func (e *Engine) Done() <-chan struct{} { return e.done }

// Stats is a snapshot of the engine's progress.
type Stats struct {
	Have, Pieces, Peers  int
	Left                 int64 // bytes still missing, for tracker announces
	Downloaded, Uploaded int64 // bytes transferred so far
}

// Stats returns the current progress.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Stats{Have: e.haveCount, Pieces: e.meta.NumPieces(), Peers: len(e.peers),
		Downloaded: e.downloaded, Uploaded: e.uploaded}
	for i := range e.avail {
		if !e.have.Has(i) {
			size, _ := e.meta.PieceSize(i)
			s.Left += size
		}
	}
	return s
}

// Run reassigns stalled pieces and rechokes until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	stalls := time.NewTicker(time.Second)
	defer stalls.Stop()
	rechoke := time.NewTicker(e.rechokeEvery)
	defer rechoke.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-stalls.C:
			e.reassignStalled(now)
		case <-rechoke.C:
			e.rechoke()
		}
	}
}

func (e *Engine) Connected(c *peer.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.peers[c] = &peerState{has: NewBitfield(e.meta.NumPieces())}
	// Sent under the lock so no Have broadcast can get ahead of it: the
	// Bitfield must be the first message.
	if e.haveCount > 0 {
		c.Send(wire.Message{ID: wire.Bitfield, Data: e.have.Clone()})
	}
}

func (e *Engine) Disconnected(c *peer.Conn, _ error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ps := e.peers[c]
	if ps == nil {
		return
	}
	for i := range e.avail {
		if ps.has.Has(i) {
			e.avail[i]--
		}
	}
	e.releaseOwnedBy(c)
	delete(e.peers, c)
	if e.optimistic == c {
		e.optimistic = nil
	}
	e.fillSlots()
	e.requestFromAll() // others may now take over the released pieces
}

func (e *Engine) Message(c *peer.Conn, m wire.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ps := e.peers[c]
	if ps == nil {
		return
	}
	switch m.ID {
	case wire.Bitfield:
		for i := range e.avail {
			if Bitfield(m.Data).Has(i) {
				ps.has.Set(i)
				e.avail[i]++
			}
		}
		e.updateInterest(c, ps)
	case wire.Have:
		if i := int(m.Index); !ps.has.Has(i) {
			ps.has.Set(i)
			e.avail[i]++
			e.updateInterest(c, ps)
		}
	case wire.Choke:
		e.releaseOwnedBy(c)
		e.requestFromAll()
	case wire.Interested:
		e.fillSlots() // contributors first, so a free-rider cannot jump the queue
	case wire.NotInterested:
		e.fillSlots()
	case wire.Request:
		e.serve(c, m)
	case wire.Piece:
		e.receive(c, ps, m)
	}
	// Unchoke, Have, Bitfield and Piece can all open room for more requests.
	e.requestMore(c, ps)
}

// serve answers a request at once, so Cancel has nothing to withdraw and is
// ignored. Requests are bounded by the peer's send queue (peer.Conn.Send).
func (e *Engine) serve(c *peer.Conn, m wire.Message) {
	if !e.have.Has(int(m.Index)) {
		c.Strike("requested a piece we never announced")
		return
	}
	data, err := e.store.ReadBlock(int(m.Index), int64(m.Begin), int64(m.Length))
	if err != nil {
		slog.Error("read block", "index", m.Index, "err", err)
		return
	}
	c.Send(wire.Message{ID: wire.Piece, Index: m.Index, Begin: m.Begin, Data: data})
	e.peers[c].roundUp += int64(len(data))
	e.uploaded += int64(len(data))
}

func (e *Engine) receive(c *peer.Conn, ps *peerState, m wire.Message) {
	d := e.active[int(m.Index)]
	b := int(m.Begin / wire.BlockSize)
	if d == nil || d.owner != c || b >= d.next || d.got[b] {
		return // late reply to a released piece, or a block we never asked for
	}
	copy(d.data[m.Begin:], m.Data)
	d.got[b] = true
	d.missing--
	ps.roundDown += int64(len(m.Data))
	ps.totalDown += int64(len(m.Data))
	e.downloaded += int64(len(m.Data))
	d.touched = time.Now()
	ps.inflight--
	if d.missing > 0 {
		return
	}

	delete(e.active, d.index)
	if !e.meta.VerifyPiece(d.index, d.data) {
		c.Strike(fmt.Sprintf("piece %d failed its hash check", d.index))
		return // the piece goes back to the pool
	}
	if err := e.store.WritePiece(d.index, d.data); err != nil {
		slog.Error("write piece", "index", d.index, "err", err)
		return
	}
	e.have.Set(d.index)
	e.haveCount++
	if e.cfg.OnPiece != nil {
		e.cfg.OnPiece(d.index, int64(len(d.data)))
	}
	for p, s := range e.peers {
		p.Send(wire.Message{ID: wire.Have, Index: uint32(d.index)})
		e.updateInterest(p, s)
	}
	e.checkComplete()
}

// requestMore fills c's pipeline: first with blocks of pieces it already
// owns, then by claiming new pieces.
func (e *Engine) requestMore(c *peer.Conn, ps *peerState) {
	if c.PeerChoking() {
		return
	}
	for ps.inflight < e.cfg.Pipeline {
		d := e.ownedWithBlocksLeft(c)
		if d == nil {
			i, ok := e.pickPiece(ps.has)
			if !ok {
				return
			}
			size, _ := e.meta.PieceSize(i)
			blocks := int((size + wire.BlockSize - 1) / wire.BlockSize)
			d = &download{index: i, owner: c, data: make([]byte, size), got: make([]bool, blocks), missing: blocks}
			e.active[i] = d
		}
		begin := int64(d.next) * wire.BlockSize
		length := min(wire.BlockSize, int64(len(d.data))-begin)
		c.Send(wire.Message{ID: wire.Request, Index: uint32(d.index), Begin: uint32(begin), Length: uint32(length)})
		d.next++
		d.touched = time.Now()
		ps.inflight++
	}
}

func (e *Engine) requestFromAll() {
	for c, ps := range e.peers {
		e.requestMore(c, ps)
	}
}

func (e *Engine) ownedWithBlocksLeft(c *peer.Conn) *download {
	for _, d := range e.active {
		if d.owner == c && d.next < len(d.got) {
			return d
		}
	}
	return nil
}

// pickPiece returns a piece peerHas offers that we lack and nobody is
// downloading. Ties (all candidates for Random) are broken uniformly at
// random in one pass (reservoir sampling).
func (e *Engine) pickPiece(peerHas Bitfield) (int, bool) {
	best, ties := -1, 0
	for i := range e.avail {
		if !peerHas.Has(i) || e.have.Has(i) || e.active[i] != nil {
			continue
		}
		switch {
		case e.cfg.Strategy == Sequential:
			return i, true
		case e.cfg.Strategy == RarestFirst && (best < 0 || e.avail[i] < e.avail[best]):
			best, ties = i, 1
		case e.cfg.Strategy == Random || e.avail[i] == e.avail[best]:
			if ties++; rand.IntN(ties) == 0 {
				best = i
			}
		}
	}
	return best, best >= 0
}

// releaseOwnedBy drops c's partial pieces so other peers can take them.
func (e *Engine) releaseOwnedBy(c *peer.Conn) {
	for _, d := range e.active {
		if d.owner == c {
			e.release(d)
		}
	}
}

func (e *Engine) release(d *download) {
	received := len(d.got) - d.missing
	if ps := e.peers[d.owner]; ps != nil {
		ps.inflight -= d.next - received
	}
	delete(e.active, d.index)
}

// reassignStalled cancels pieces whose owner made no progress within the
// request timeout, strikes that owner, and offers the pieces to the other
// peers. Skipping the staller matters: otherwise it can win them straight back.
func (e *Engine) reassignStalled(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stalled := map[*peer.Conn]bool{}
	for _, d := range e.active {
		if now.Sub(d.touched) < e.requestTimeout {
			continue
		}
		for b := 0; b < d.next; b++ {
			if !d.got[b] {
				begin := int64(b) * wire.BlockSize
				length := min(wire.BlockSize, int64(len(d.data))-begin)
				d.owner.Send(wire.Message{ID: wire.Cancel, Index: uint32(d.index), Begin: uint32(begin), Length: uint32(length)})
			}
		}
		stalled[d.owner] = true
		e.release(d)
	}
	for c, ps := range e.peers {
		if stalled[c] {
			c.Strike("request timed out")
		} else {
			e.requestMore(c, ps)
		}
	}
}

// updateInterest tells c whether it has any piece we still lack.
func (e *Engine) updateInterest(c *peer.Conn, ps *peerState) {
	want := false
	for i := range e.avail {
		if ps.has.Has(i) && !e.have.Has(i) {
			want = true
			break
		}
	}
	if want != c.AmInterested() {
		id := wire.NotInterested
		if want {
			id = wire.Interested
		}
		c.Send(wire.Message{ID: id})
	}
}

func (e *Engine) checkComplete() {
	if e.haveCount < e.meta.NumPieces() {
		return
	}
	select {
	case <-e.done:
		return
	default:
	}
	if err := e.store.Finish(); err != nil {
		slog.Error("finish download", "err", err)
		return
	}
	close(e.done)
}
