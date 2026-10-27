// Choking: tit-for-tat choice of the peers we upload to, the optimistic unchoke, and filling free upload slots at once.
// Owner: Swayam Kanjilal (Choking & Experiments)

package piece

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/sneharam543/shardswarm/internal/peer"
	"github.com/sneharam543/shardswarm/internal/wire"
)

// ChokePolicy decides which interested peers may download from us.
type ChokePolicy string

const (
	// TitForTat unchokes the peers that gave us the most in the last round
	// (as a seeder: that took the most), plus one optimistic unchoke.
	TitForTat ChokePolicy = "tft"
	// ChokeOff unchokes every interested peer: the experiment baseline.
	ChokeOff ChokePolicy = "off"
	// FreeRider never unchokes anyone, so it never uploads.
	FreeRider ChokePolicy = "freerider"
)

const (
	regularSlots     = 3 // peers unchoked for reciprocating
	optimisticRounds = 3 // a new optimistic unchoke every 3 rechokes (30 s)
)

// ParseChokePolicy accepts "tft", "off" or "freerider".
func ParseChokePolicy(s string) (ChokePolicy, error) {
	switch p := ChokePolicy(s); p {
	case TitForTat, ChokeOff, FreeRider:
		return p, nil
	}
	return "", fmt.Errorf("unknown choke policy %q (want tft, off or freerider)", s)
}

// score ranks a peer for an upload slot: bytes this round first, then
// bytes over the whole connection. The second key keeps a contributor that
// had a quiet round ahead of a peer that never gave anything.
type score struct{ round, total int64 }

// ranked returns the keys of scores best first; equal scores in random order.
func ranked[K comparable](scores map[K]score) []K {
	keys := make([]K, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := scores[keys[i]], scores[keys[j]]
		return a.round > b.round || a.round == b.round && a.total > b.total
	})
	return keys
}

// pickUnchoked chooses whom to unchoke among the interested peers in rates.
// The top slots by score are unchoked. One more peer is unchoked
// optimistically: the previous one is kept unless rotate is set or it no
// longer qualifies. A peer that never uploads can only get this slot.
func pickUnchoked[K comparable](rates map[K]score, prevOptimistic K, rotate bool, slots int) (map[K]bool, K) {
	keys := ranked(rates)

	n := min(slots, len(keys))
	unchoked := make(map[K]bool, n+1)
	for _, k := range keys[:n] {
		unchoked[k] = true
	}
	rest := keys[n:]

	optimistic := prevOptimistic
	if _, interested := rates[optimistic]; rotate || !interested || unchoked[optimistic] {
		var none K
		optimistic = none
		if len(rest) > 0 {
			optimistic = rest[rand.IntN(len(rest))]
		}
	}
	if _, ok := rates[optimistic]; ok {
		unchoked[optimistic] = true
	}
	return unchoked, optimistic
}

// rechoke runs every round. Under TitForTat it recomputes the unchoked set
// from this round's byte counts; every policy resets the counts.
func (e *Engine) rechoke() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.round++
	seeding := e.haveCount == e.meta.NumPieces()
	rates := make(map[*peer.Conn]score)
	for c, ps := range e.peers {
		if c.PeerInterested() {
			rates[c] = score{ps.roundDown, ps.totalDown}
			if seeding {
				rates[c] = score{round: ps.roundUp}
			}
		}
		ps.roundDown, ps.roundUp = 0, 0
	}
	if e.cfg.Choke != TitForTat {
		return // off and freerider decide on each Interested message
	}

	var unchoked map[*peer.Conn]bool
	unchoked, e.optimistic = pickUnchoked(rates, e.optimistic, e.round%optimisticRounds == 0, regularSlots)
	for c := range e.peers {
		switch {
		case unchoked[c] && c.AmChoking():
			c.Send(wire.Message{ID: wire.Unchoke})
		case !unchoked[c] && !c.AmChoking():
			c.Send(wire.Message{ID: wire.Choke})
		}
	}
}

// unchokeNow reports whether a newly interested peer may be unchoked
// without waiting for the next round.
func (e *Engine) unchokeNow() bool {
	switch e.cfg.Choke {
	case ChokeOff:
		return true
	case FreeRider:
		return false
	}
	// TitForTat: fill free slots at once, so newcomers do not idle for a
	// whole round; the next rechoke reorders them by rate. A peer that is
	// no longer interested does not hold a slot.
	unchoked := 0
	for c := range e.peers {
		if !c.AmChoking() && c.PeerInterested() {
			unchoked++
		}
	}
	return unchoked < regularSlots+1
}

// fillSlots unchokes waiting interested peers into free slots at once,
// instead of leaving the slots idle until the next rechoke. Peers that have
// given us the most go first, so a free-rider only gets a slot nobody who
// contributes is waiting for.
func (e *Engine) fillSlots() {
	waiting := make(map[*peer.Conn]score)
	for c, ps := range e.peers {
		if c.AmChoking() && c.PeerInterested() {
			waiting[c] = score{total: ps.totalDown}
		}
	}
	for _, c := range ranked(waiting) {
		if !e.unchokeNow() {
			return
		}
		c.Send(wire.Message{ID: wire.Unchoke})
	}
}
