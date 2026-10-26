// Owner: Aditya Giri (Peer Wire Protocol)

// Package wire encodes and decodes the ShardSwarm peer protocol. The format
// is specified in docs/PROTOCOL.md.
package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/sneharam543/shardswarm/internal/metainfo"
)

// BlockSize is the unit of Request and Piece. Every block is BlockSize bytes
// except the last block of a piece, which may be shorter.
const BlockSize = 16 << 10

const protocolName = "SHARDSWARM1"

// handshakeLen is the fixed size of a handshake.
const handshakeLen = 1 + len(protocolName) + 32 + 20

// ID identifies a message type.
type ID uint8

const (
	Choke ID = iota
	Unchoke
	Interested
	NotInterested
	Have
	Bitfield
	Request
	Piece
	Cancel

	// KeepAlive is never sent as an ID; on the wire it is a zero-length frame.
	KeepAlive ID = 0xFF
)

// ErrInvalid marks a message that was framed correctly but breaks a protocol
// rule. Its bytes were consumed, so the stream is still usable: the caller
// may count a strike and keep reading. Any other error from Read means the
// connection must be closed.
var ErrInvalid = errors.New("invalid message")

// Message is any message other than the handshake. Only the fields used by
// its ID are meaningful.
type Message struct {
	ID     ID
	Index  uint32 // Have, Request, Piece, Cancel
	Begin  uint32 // Request, Piece, Cancel
	Length uint32 // Request, Cancel
	Data   []byte // Bitfield bits, or the Piece block
}

// WriteHandshake sends the handshake for the swarm infoHash.
func WriteHandshake(w io.Writer, infoHash [32]byte, peerID [20]byte) error {
	buf := make([]byte, 0, handshakeLen)
	buf = append(buf, byte(len(protocolName)))
	buf = append(buf, protocolName...)
	buf = append(buf, infoHash[:]...)
	buf = append(buf, peerID[:]...)
	_, err := w.Write(buf)
	return err
}

// ReadHandshake reads the remote handshake and returns the remote peer ID.
// It fails if the remote speaks another protocol, is in another swarm, or is
// this peer itself (self is our own peer ID).
func ReadHandshake(r io.Reader, infoHash [32]byte, self [20]byte) ([20]byte, error) {
	var buf [handshakeLen]byte
	var remote [20]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return remote, fmt.Errorf("read handshake: %w", err)
	}
	if buf[0] != byte(len(protocolName)) || string(buf[1:1+len(protocolName)]) != protocolName {
		return remote, errors.New("handshake: not a ShardSwarm peer")
	}
	if !bytes.Equal(buf[1+len(protocolName):1+len(protocolName)+32], infoHash[:]) {
		return remote, errors.New("handshake: peer is in a different swarm")
	}
	copy(remote[:], buf[handshakeLen-20:])
	if remote == self {
		return remote, errors.New("handshake: connected to self")
	}
	return remote, nil
}

// Write sends m as one frame in a single Write call.
func Write(w io.Writer, m Message) error {
	buf, err := m.encode()
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// Read reads one frame and checks it against meta. A clean close between
// frames returns io.EOF. See ErrInvalid for which errors are recoverable.
func Read(r io.Reader, meta *metainfo.MetaInfo) (Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Message{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return Message{ID: KeepAlive}, nil
	}
	// Check the length before allocating: it comes straight from the peer.
	if limit := maxFrame(meta.NumPieces()); n > limit {
		return Message{}, fmt.Errorf("frame length %d exceeds limit %d", n, limit)
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(r, frame); err != nil {
		return Message{}, fmt.Errorf("read frame: %w", err)
	}
	m, err := decode(frame)
	if err != nil {
		return Message{}, err
	}
	if err := m.check(meta); err != nil {
		return Message{}, err
	}
	return m, nil
}

func maxFrame(numPieces int) uint32 {
	return uint32(max(1+8+BlockSize, 1+(numPieces+7)/8))
}

func (m Message) encode() ([]byte, error) {
	be := binary.BigEndian
	var payload []byte
	switch m.ID {
	case KeepAlive:
		return make([]byte, 4), nil
	case Choke, Unchoke, Interested, NotInterested:
	case Have:
		payload = be.AppendUint32(nil, m.Index)
	case Bitfield:
		payload = m.Data
	case Request, Cancel:
		payload = be.AppendUint32(be.AppendUint32(be.AppendUint32(nil, m.Index), m.Begin), m.Length)
	case Piece:
		payload = append(be.AppendUint32(be.AppendUint32(make([]byte, 0, 8+len(m.Data)), m.Index), m.Begin), m.Data...)
	default:
		return nil, fmt.Errorf("encode: unknown message id %d", m.ID)
	}
	buf := make([]byte, 0, 5+len(payload))
	buf = be.AppendUint32(buf, uint32(1+len(payload)))
	buf = append(buf, byte(m.ID))
	return append(buf, payload...), nil
}

func decode(frame []byte) (Message, error) {
	m := Message{ID: ID(frame[0])}
	p := frame[1:]
	be := binary.BigEndian
	var sizeOK bool
	switch m.ID {
	case Choke, Unchoke, Interested, NotInterested:
		sizeOK = len(p) == 0
	case Have:
		if sizeOK = len(p) == 4; sizeOK {
			m.Index = be.Uint32(p)
		}
	case Bitfield:
		sizeOK, m.Data = true, p
	case Request, Cancel:
		if sizeOK = len(p) == 12; sizeOK {
			m.Index, m.Begin, m.Length = be.Uint32(p), be.Uint32(p[4:]), be.Uint32(p[8:])
		}
	case Piece:
		if sizeOK = len(p) >= 8; sizeOK {
			m.Index, m.Begin, m.Data = be.Uint32(p), be.Uint32(p[4:]), p[8:]
		}
	default:
		return m, fmt.Errorf("%w: unknown id %d", ErrInvalid, m.ID)
	}
	if !sizeOK {
		return m, fmt.Errorf("%w: id %d with %d-byte payload", ErrInvalid, m.ID, len(p))
	}
	return m, nil
}

// check enforces the rules that need the metainfo: piece indices in range,
// exact bitfield size, and blocks on the BlockSize grid.
func (m Message) check(meta *metainfo.MetaInfo) error {
	numPieces := meta.NumPieces()
	switch m.ID {
	case Have:
		if uint64(m.Index) >= uint64(numPieces) {
			return fmt.Errorf("%w: have index %d out of range", ErrInvalid, m.Index)
		}
	case Bitfield:
		if len(m.Data) != (numPieces+7)/8 {
			return fmt.Errorf("%w: bitfield is %d bytes, want %d", ErrInvalid, len(m.Data), (numPieces+7)/8)
		}
		if spare := numPieces % 8; spare != 0 && m.Data[len(m.Data)-1]&(0xFF>>spare) != 0 {
			return fmt.Errorf("%w: bitfield spare bits set", ErrInvalid)
		}
	case Request, Cancel:
		return checkBlock(meta, m.Index, m.Begin, uint64(m.Length))
	case Piece:
		return checkBlock(meta, m.Index, m.Begin, uint64(len(m.Data)))
	}
	return nil
}

// checkBlock accepts exactly the blocks of the grid: begin is a multiple of
// BlockSize inside the piece, and length is the full block (shorter only at
// the piece's end). Arithmetic is in uint64 so peer values cannot overflow it.
func checkBlock(meta *metainfo.MetaInfo, index, begin uint32, length uint64) error {
	if uint64(index) >= uint64(meta.NumPieces()) {
		return fmt.Errorf("%w: piece index %d out of range", ErrInvalid, index)
	}
	size, _ := meta.PieceSize(int(index))
	if begin%BlockSize != 0 || uint64(begin) >= uint64(size) {
		return fmt.Errorf("%w: block begin %d invalid for piece %d", ErrInvalid, begin, index)
	}
	if want := min(BlockSize, uint64(size)-uint64(begin)); length != want {
		return fmt.Errorf("%w: block length %d, want %d", ErrInvalid, length, want)
	}
	return nil
}
