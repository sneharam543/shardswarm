// Owner: Rishav Sarkar (Download Engine & Concurrency)

// Package piece holds the download engine: piece selection, request
// pipelining, hash verification and the on-disk file.
package piece

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/sneharam543/shardswarm/internal/metainfo"
)

// Bitfield is a set of piece indices, packed as on the wire: bit 7 of byte 0
// is piece 0.
type Bitfield []byte

func NewBitfield(n int) Bitfield   { return make(Bitfield, (n+7)/8) }
func (b Bitfield) Has(i int) bool  { return b[i/8]&(0x80>>(i%8)) != 0 }
func (b Bitfield) Set(i int)       { b[i/8] |= 0x80 >> (i % 8) }
func (b Bitfield) Clone() Bitfield { return append(Bitfield(nil), b...) }

// Store is the file being shared. Downloads go to "<name>.part" inside the
// download directory and are renamed to "<name>" once every piece is in.
// It is safe for concurrent use.
type Store struct {
	meta        *metainfo.MetaInfo
	final, part string // final name, and the name used while downloading

	mu       sync.RWMutex // write-locked only while swapping f in Finish
	f        *os.File
	complete bool
}

// OpenStore opens the file for meta in dir and returns which pieces it
// already holds, each checked against its hash. An existing final file is
// seeded only if every piece matches; it is never modified or overwritten.
func OpenStore(dir string, meta *metainfo.MetaInfo) (*Store, Bitfield, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	// The name passed metainfo validation, so it cannot leave dir.
	final := filepath.Join(dir, meta.Info.Name)
	s := &Store{meta: meta, final: final, part: final + ".part"}

	if f, err := os.Open(final); err == nil {
		have, n := verify(f, meta)
		if n != meta.NumPieces() {
			f.Close()
			return nil, nil, fmt.Errorf("%s exists but does not match the metainfo (%d of %d pieces); refusing to overwrite it",
				final, n, meta.NumPieces())
		}
		s.f, s.complete = f, true
		return s, have, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}

	// ponytail: no free-space check (no portable stdlib call); Truncate makes
	// a sparse file and a full disk surfaces as a WritePiece error.
	f, err := os.OpenFile(s.part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, err
	}
	if err := f.Truncate(meta.Info.Length); err != nil {
		f.Close()
		return nil, nil, err
	}
	s.f = f
	have, _ := verify(f, meta) // resume: keep whatever pieces are already valid
	return s, have, nil
}

// verify returns the pieces of f that match their hashes, and how many.
func verify(f *os.File, meta *metainfo.MetaInfo) (Bitfield, int) {
	have := NewBitfield(meta.NumPieces())
	count := 0
	buf := make([]byte, meta.Info.PieceLength)
	for i := 0; i < meta.NumPieces(); i++ {
		off, _ := meta.PieceOffset(i)
		size, _ := meta.PieceSize(i)
		if _, err := f.ReadAt(buf[:size], off); err == nil && meta.VerifyPiece(i, buf[:size]) {
			have.Set(i)
			count++
		}
	}
	return have, count
}

// ReadBlock reads length bytes at begin within piece index. The caller has
// already checked the range against the metainfo (wire.Read does).
func (s *Store) ReadBlock(index int, begin, length int64) ([]byte, error) {
	off, err := s.meta.PieceOffset(index)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, length)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.f.ReadAt(buf, off+begin); err != nil {
		return nil, err
	}
	return buf, nil
}

// WritePiece writes a verified piece at its offset.
func (s *Store) WritePiece(index int, data []byte) error {
	off, err := s.meta.PieceOffset(index)
	if err != nil {
		return err
	}
	s.mu.RLock() // ReadAt/WriteAt are safe concurrently; the lock guards s.f
	defer s.mu.RUnlock()
	if s.complete {
		return errors.New("store is complete")
	}
	_, err = s.f.WriteAt(data, off)
	return err
}

// Finish flushes the .part file and renames it to its final name, then
// reopens it read-only for seeding. Calling it again does nothing.
func (s *Store) Finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.complete {
		return nil
	}
	if _, err := os.Lstat(s.final); err == nil {
		return fmt.Errorf("%s appeared during the download; refusing to overwrite it", s.final)
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	if err := s.f.Close(); err != nil { // Windows cannot rename an open file
		return err
	}
	if err := os.Rename(s.part, s.final); err != nil {
		// Keep seeding from the .part file rather than from a closed handle.
		if f, rerr := os.Open(s.part); rerr == nil {
			s.f = f
		}
		return err
	}
	f, err := os.Open(s.final)
	if err != nil {
		return err
	}
	s.f, s.complete = f, true
	return nil
}

// Close releases the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
