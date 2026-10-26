// Tests for the wire format: exact bytes, round trips, invalid and fatal frames, handshakes, and fuzzing of Read.
// Owner: Aditya Giri (Peer Wire Protocol)

package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sneharam543/shardswarm/internal/metainfo"
)

// testMeta has 10 pieces of 32 KiB (two blocks each), except the last piece,
// which is 20000 bytes: one full block and one 3616-byte block.
func testMeta(t testing.TB) *metainfo.MetaInfo {
	t.Helper()
	const pieceLen = 2 * BlockSize
	m, err := metainfo.Build(bytes.NewReader(make([]byte, 9*pieceLen+20000)), "f.bin", pieceLen)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// frame builds a raw frame: 4-byte length, then id and payload.
func frame(id byte, payload ...byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(1+len(payload)))
	return append(append(b, id), payload...)
}

func u32s(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

func encodeT(t *testing.T, m Message) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, m); err != nil {
		t.Fatalf("Write(%+v): %v", m, err)
	}
	return buf.Bytes()
}

func TestHandshake(t *testing.T) {
	t.Parallel()
	var swarm, other [32]byte
	swarm[0], other[0] = 1, 2
	me, them := [20]byte{1}, [20]byte{2}

	var buf bytes.Buffer
	if err := WriteHandshake(&buf, swarm, them); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	if len(good) != handshakeLen || good[0] != 11 || string(good[1:12]) != "SHARDSWARM1" {
		t.Fatalf("handshake bytes = % x", good)
	}

	got, err := ReadHandshake(bytes.NewReader(good), swarm, me)
	if err != nil || got != them {
		t.Fatalf("ReadHandshake = %x, %v; want %x", got, err, them)
	}

	mutate := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	tests := []struct {
		name     string
		in       []byte
		infoHash [32]byte
		self     [20]byte
	}{
		{"wrong name length", mutate(func(b []byte) []byte { b[0] = 19; return b }), swarm, me},
		{"wrong protocol", mutate(func(b []byte) []byte { b[1] = 'X'; return b }), swarm, me},
		{"different swarm", good, other, me},
		{"connected to self", good, swarm, them},
		{"truncated", good[:handshakeLen-1], swarm, me},
		{"empty", nil, swarm, me},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadHandshake(bytes.NewReader(tt.in), tt.infoHash, tt.self); err == nil {
				t.Error("ReadHandshake succeeded, want error")
			}
		})
	}
}

func TestWrite_ExactBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		m    Message
		want []byte
	}{
		{"keep-alive", Message{ID: KeepAlive}, []byte{0, 0, 0, 0}},
		{"choke", Message{ID: Choke}, []byte{0, 0, 0, 1, 0}},
		{"have", Message{ID: Have, Index: 0x01020304}, []byte{0, 0, 0, 5, 4, 1, 2, 3, 4}},
		{"request", Message{ID: Request, Index: 1, Begin: 0x4000, Length: 0x4000},
			[]byte{0, 0, 0, 13, 6, 0, 0, 0, 1, 0, 0, 0x40, 0, 0, 0, 0x40, 0}},
		{"piece", Message{ID: Piece, Index: 2, Begin: 0, Data: []byte{0xAA, 0xBB}},
			[]byte{0, 0, 0, 11, 7, 0, 0, 0, 2, 0, 0, 0, 0, 0xAA, 0xBB}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := encodeT(t, tt.m); !bytes.Equal(got, tt.want) {
				t.Errorf("got % x\nwant % x", got, tt.want)
			}
		})
	}
}

func TestWrite_UnknownID(t *testing.T) {
	t.Parallel()
	if err := Write(io.Discard, Message{ID: 42}); err == nil {
		t.Error("Write accepted an unknown id")
	}
}

func TestRead_RoundTrip(t *testing.T) {
	t.Parallel()
	meta := testMeta(t)
	block := bytes.Repeat([]byte{7}, BlockSize)
	tests := []struct {
		name string
		m    Message
	}{
		{"keep-alive", Message{ID: KeepAlive}},
		{"choke", Message{ID: Choke}},
		{"unchoke", Message{ID: Unchoke}},
		{"interested", Message{ID: Interested}},
		{"not interested", Message{ID: NotInterested}},
		{"have last piece", Message{ID: Have, Index: 9}},
		{"bitfield all pieces", Message{ID: Bitfield, Data: []byte{0xFF, 0xC0}}},
		{"request first block", Message{ID: Request, Index: 0, Begin: 0, Length: BlockSize}},
		{"request short last block", Message{ID: Request, Index: 9, Begin: BlockSize, Length: 3616}},
		{"cancel", Message{ID: Cancel, Index: 3, Begin: BlockSize, Length: BlockSize}},
		{"piece full block", Message{ID: Piece, Index: 4, Begin: BlockSize, Data: block}},
		{"piece short last block", Message{ID: Piece, Index: 9, Begin: BlockSize, Data: block[:3616]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Read(bytes.NewReader(encodeT(t, tt.m)), meta)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.ID != tt.m.ID || got.Index != tt.m.Index || got.Begin != tt.m.Begin ||
				got.Length != tt.m.Length || !bytes.Equal(got.Data, tt.m.Data) {
				t.Errorf("got %+v, want %+v", got, tt.m)
			}
		})
	}
}

// Invalid messages are consumed whole, so the next frame must still decode.
func TestRead_InvalidKeepsStreamUsable(t *testing.T) {
	t.Parallel()
	meta := testMeta(t)
	tests := []struct {
		name string
		raw  []byte
	}{
		{"unknown id", frame(99, 1, 2, 3)},
		{"choke with payload", frame(byte(Choke), 0)},
		{"have too short", frame(byte(Have), 0, 0, 1)},
		{"request too long", frame(byte(Request), u32s(0, 0, BlockSize, 0)...)},
		{"piece without header", frame(byte(Piece), 0, 0, 0)},
		{"have index out of range", frame(byte(Have), u32s(10)...)},
		{"have index max uint32", frame(byte(Have), u32s(0xFFFFFFFF)...)},
		{"bitfield too short", frame(byte(Bitfield), 0xFF)},
		{"bitfield too long", frame(byte(Bitfield), 0xFF, 0xC0, 0)},
		{"bitfield spare bit set", frame(byte(Bitfield), 0xFF, 0xE0)},
		{"request index out of range", frame(byte(Request), u32s(10, 0, BlockSize)...)},
		{"request unaligned begin", frame(byte(Request), u32s(0, 1, BlockSize)...)},
		{"request begin past piece", frame(byte(Request), u32s(0, 2*BlockSize, BlockSize)...)},
		{"request begin overflow", frame(byte(Request), u32s(0, 0xFFFFC000, BlockSize)...)},
		{"request partial block", frame(byte(Request), u32s(0, 0, 100)...)},
		{"request oversized block", frame(byte(Request), u32s(0, 0, 2*BlockSize)...)},
		{"request zero length", frame(byte(Request), u32s(0, 0, 0)...)},
		{"request last block too long", frame(byte(Request), u32s(9, BlockSize, BlockSize)...)},
		{"cancel out of range", frame(byte(Cancel), u32s(10, 0, BlockSize)...)},
		{"piece block too short", frame(byte(Piece), append(u32s(0, 0), 1, 2, 3)...)},
		{"piece empty block", frame(byte(Piece), u32s(0, 0)...)},
	}
	next := frame(byte(Have), u32s(5)...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := bytes.NewReader(append(append([]byte(nil), tt.raw...), next...))
			if _, err := Read(r, meta); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if m, err := Read(r, meta); err != nil || m.ID != Have || m.Index != 5 {
				t.Errorf("next message = %+v, %v; want have 5", m, err)
			}
		})
	}
}

// Fatal errors mean the stream cannot be trusted; they must not be ErrInvalid.
func TestRead_FatalErrors(t *testing.T) {
	t.Parallel()
	meta := testMeta(t)
	oversized := binary.BigEndian.AppendUint32(nil, 1+8+BlockSize+1)
	tests := []struct {
		name    string
		raw     []byte
		wantErr error
	}{
		{"clean close", nil, io.EOF},
		{"truncated length", []byte{0, 0}, io.ErrUnexpectedEOF},
		{"truncated payload", frame(byte(Have), 0, 0)[:6], io.ErrUnexpectedEOF},
		{"oversized frame", oversized, nil},
		{"huge frame", []byte{0xFF, 0xFF, 0xFF, 0xFF}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Read(bytes.NewReader(tt.raw), meta)
			if err == nil || errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want a fatal error", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !strings.Contains(err.Error(), "exceeds limit") {
				t.Errorf("err = %v, want length-limit error", err)
			}
		})
	}
}

func TestMaxFrame_LargeBitfield(t *testing.T) {
	t.Parallel()
	// 4 GiB at 16 KiB pieces: the bitfield frame outgrows a piece frame.
	if got, want := maxFrame(262144), uint32(1+262144/8); got != want {
		t.Errorf("maxFrame = %d, want %d", got, want)
	}
	if got, want := maxFrame(10), uint32(1+8+BlockSize); got != want {
		t.Errorf("maxFrame = %d, want %d", got, want)
	}
}

// FuzzRead checks that no input panics and that every accepted message
// re-encodes to exactly the bytes it was read from.
func FuzzRead(f *testing.F) {
	meta := testMeta(f)
	f.Add(frame(byte(Have), u32s(3)...))
	f.Add(frame(byte(Request), u32s(9, BlockSize, 3616)...))
	f.Add(frame(byte(Bitfield), 0xFF, 0xC0))
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 7})

	f.Fuzz(func(t *testing.T, in []byte) {
		r := bytes.NewReader(in)
		m, err := Read(r, meta)
		if err != nil {
			return
		}
		consumed := in[:len(in)-r.Len()]
		var out bytes.Buffer
		if err := Write(&out, m); err != nil {
			t.Fatalf("re-encode %+v: %v", m, err)
		}
		if !bytes.Equal(out.Bytes(), consumed) {
			t.Fatalf("round trip changed bytes:\n in % x\nout % x", consumed, out.Bytes())
		}
	})
}
