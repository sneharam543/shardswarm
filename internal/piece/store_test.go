// Tests for the store and bitfield: writing and renaming the download, resuming, seeding, refusing to overwrite, and empty files.
// Owner: Rishav Sarkar (Download Engine & Concurrency)

package piece

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/sneharam543/shardswarm/internal/metainfo"
	"github.com/sneharam543/shardswarm/internal/wire"
)

const pieceLen = 2 * wire.BlockSize

// testFile returns random data of 9.375 pieces (the last piece is one short
// block) and its metainfo.
func testFile(t testing.TB) ([]byte, *metainfo.MetaInfo) {
	t.Helper()
	data := make([]byte, 9*pieceLen+12288)
	rand.New(rand.NewSource(1)).Read(data)
	meta, err := metainfo.Build(bytes.NewReader(data), "movie.bin", pieceLen)
	if err != nil {
		t.Fatal(err)
	}
	return data, meta
}

func count(b Bitfield, n int) int {
	c := 0
	for i := 0; i < n; i++ {
		if b.Has(i) {
			c++
		}
	}
	return c
}

func TestBitfield(t *testing.T) {
	t.Parallel()
	b := NewBitfield(10)
	if len(b) != 2 {
		t.Fatalf("len = %d, want 2", len(b))
	}
	b.Set(0)
	b.Set(9)
	if !b.Has(0) || !b.Has(9) || b.Has(1) || b.Has(8) {
		t.Errorf("bits = % x", b)
	}
	if b[0] != 0x80 || b[1] != 0x40 {
		t.Errorf("wire layout = % x, want 80 40", b)
	}
	c := b.Clone()
	c.Set(1)
	if b.Has(1) {
		t.Error("Clone shares memory")
	}
}

func TestStore_DownloadThenFinish(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	dir := t.TempDir()
	s, have, err := OpenStore(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if count(have, meta.NumPieces()) != 0 {
		t.Fatal("fresh store claims pieces")
	}
	if fi, err := os.Stat(filepath.Join(dir, "movie.bin.part")); err != nil || fi.Size() != meta.Info.Length {
		t.Fatalf("part file: %v, %v", fi, err)
	}

	for i := meta.NumPieces() - 1; i >= 0; i-- { // out of order on purpose
		off, _ := meta.PieceOffset(i)
		size, _ := meta.PieceSize(i)
		if err := s.WritePiece(i, data[off:off+size]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(); err != nil {
		t.Errorf("second Finish: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "movie.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("final file differs from source (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.bin.part")); !os.IsNotExist(err) {
		t.Error(".part file left behind")
	}
	if blk, err := s.ReadBlock(9, 0, 12288); err != nil || !bytes.Equal(blk, data[9*pieceLen:]) {
		t.Errorf("ReadBlock after Finish: %v", err)
	}
	if err := s.WritePiece(0, data[:pieceLen]); err == nil {
		t.Error("WritePiece succeeded on a finished store")
	}
}

func TestStore_ResumesValidPieces(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	dir := t.TempDir()
	part := append([]byte(nil), data...)
	part[3*pieceLen] ^= 1    // corrupt piece 3
	part = part[:7*pieceLen] // pieces 7.. never arrived
	if err := os.WriteFile(filepath.Join(dir, "movie.bin.part"), part, 0o644); err != nil {
		t.Fatal(err)
	}
	s, have, err := OpenStore(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < meta.NumPieces(); i++ {
		if want := i < 7 && i != 3; have.Has(i) != want {
			t.Errorf("piece %d: have = %v, want %v", i, have.Has(i), want)
		}
	}
}

func TestStore_SeedsMatchingFile(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "movie.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, have, err := OpenStore(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if count(have, meta.NumPieces()) != meta.NumPieces() {
		t.Error("matching file not recognised as complete")
	}
}

func TestStore_RefusesToOverwrite(t *testing.T) {
	t.Parallel()
	_, meta := testFile(t)

	t.Run("mismatched final file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "movie.bin")
		if err := os.WriteFile(path, []byte("my thesis"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenStore(dir, meta); err == nil {
			t.Fatal("OpenStore accepted an unrelated file")
		}
		if got, _ := os.ReadFile(path); string(got) != "my thesis" {
			t.Error("unrelated file was modified")
		}
	})

	t.Run("final file appears during download", func(t *testing.T) {
		dir := t.TempDir()
		s, _, err := OpenStore(dir, meta)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		path := filepath.Join(dir, "movie.bin")
		if err := os.WriteFile(path, []byte("my thesis"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(); err == nil {
			t.Fatal("Finish overwrote an existing file")
		}
		if got, _ := os.ReadFile(path); string(got) != "my thesis" {
			t.Error("existing file was modified")
		}
	})
}

// If the final rename fails, the store must still serve its data.
func TestStore_FailedRenameKeepsSeeding(t *testing.T) {
	t.Parallel()
	data, meta := testFile(t)
	dir := t.TempDir()
	s, _, err := OpenStore(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < meta.NumPieces(); i++ {
		off, _ := meta.PieceOffset(i)
		size, _ := meta.PieceSize(i)
		if err := s.WritePiece(i, data[off:off+size]); err != nil {
			t.Fatal(err)
		}
	}
	s.final = filepath.Join(dir, "missing-dir", "movie.bin") // rename target's parent does not exist
	if err := s.Finish(); err == nil {
		t.Fatal("Finish succeeded although the rename cannot")
	}
	if blk, err := s.ReadBlock(0, 0, wire.BlockSize); err != nil || !bytes.Equal(blk, data[:wire.BlockSize]) {
		t.Errorf("ReadBlock after failed Finish: %v", err)
	}
}

func TestStore_EmptyFile(t *testing.T) {
	t.Parallel()
	meta, err := metainfo.Build(bytes.NewReader(nil), "empty.txt", pieceLen)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s, _, err := OpenStore(dir, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := NewEngine(meta, s, NewBitfield(0), Config{Strategy: RarestFirst, Pipeline: 5, Choke: TitForTat})
	select {
	case <-e.Done():
	default:
		t.Fatal("empty file not complete immediately")
	}
	if fi, err := os.Stat(filepath.Join(dir, "empty.txt")); err != nil || fi.Size() != 0 {
		t.Errorf("empty output: %v, %v", fi, err)
	}
}
