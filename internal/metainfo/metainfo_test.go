// Tests for metainfo: piece layout for every file shape, SHA-256 vectors, rejection of malformed or tampered files, name validation, and fuzzing of Load.
// Owner: Sneha Ram (Metainfo & Tracker)

package metainfo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pl = MinPieceLength // small pieces keep test data small

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func mustBuild(t testing.TB, data []byte, name string) *MetaInfo {
	t.Helper()
	m, err := Build(bytes.NewReader(data), name, pl)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return m
}

// encodeRaw marshals m without validating, optionally recomputing the info
// hash so a test can isolate one invalid field from the hash check.
func encodeRaw(t *testing.T, m MetaInfo, rehash bool) []byte {
	t.Helper()
	if rehash {
		h, err := hashInfo(m.Info)
		if err != nil {
			t.Fatal(err)
		}
		m.InfoHash = hex.EncodeToString(h[:])
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		size       int
		wantPieces int
		wantLast   int64
	}{
		{name: "empty file", size: 0, wantPieces: 0},
		{name: "one byte", size: 1, wantPieces: 1, wantLast: 1},
		{name: "exactly one piece", size: int(pl), wantPieces: 1, wantLast: pl},
		{name: "exact multiple", size: 3 * int(pl), wantPieces: 3, wantLast: pl},
		{name: "partial last piece", size: 2*int(pl) + 100, wantPieces: 3, wantLast: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := randomBytes(t, tt.size)
			m := mustBuild(t, data, "file.bin")

			if m.Info.Length != int64(tt.size) {
				t.Errorf("Length = %d, want %d", m.Info.Length, tt.size)
			}
			if m.NumPieces() != tt.wantPieces {
				t.Fatalf("NumPieces = %d, want %d", m.NumPieces(), tt.wantPieces)
			}
			for i := 0; i < m.NumPieces(); i++ {
				off, _ := m.PieceOffset(i)
				size, _ := m.PieceSize(i)
				piece := data[off : off+size]
				if want := sha256.Sum256(piece); m.pieceHashes[i] != want {
					t.Errorf("piece %d hash mismatch", i)
				}
				if !m.VerifyPiece(i, piece) {
					t.Errorf("VerifyPiece(%d) = false for the original data", i)
				}
			}
			if tt.wantPieces > 0 {
				if last, _ := m.PieceSize(tt.wantPieces - 1); last != tt.wantLast {
					t.Errorf("last piece size = %d, want %d", last, tt.wantLast)
				}
			}
		})
	}
}

func TestBuild_KnownVector(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, []byte("abc"), "abc.txt")
	// FIPS 180-2 test vector for SHA-256("abc").
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := m.Info.Pieces[0]; got != want {
		t.Errorf("piece hash = %s, want %s", got, want)
	}
}

func TestBuild_InfoHashIdentifiesContent(t *testing.T) {
	t.Parallel()
	data := randomBytes(t, 5000)
	a := mustBuild(t, data, "a.bin")
	again := mustBuild(t, data, "a.bin")
	renamed := mustBuild(t, data, "b.bin")

	if a.InfoHash != again.InfoHash {
		t.Error("same input produced different info hashes")
	}
	if a.InfoHash == renamed.InfoHash {
		t.Error("different names produced the same info hash")
	}
	if got := a.InfoHashBytes(); hex.EncodeToString(got[:]) != a.InfoHash {
		t.Error("InfoHashBytes disagrees with InfoHash")
	}
}

func TestBuild_Errors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		r           io.Reader
		fileName    string
		pieceLength int64
	}{
		{name: "empty name", r: strings.NewReader("x"), fileName: "", pieceLength: pl},
		{name: "path traversal", r: strings.NewReader("x"), fileName: "../etc/passwd", pieceLength: pl},
		{name: "piece length zero", r: strings.NewReader("x"), fileName: "f", pieceLength: 0},
		{name: "piece length below minimum", r: strings.NewReader("x"), fileName: "f", pieceLength: 8 << 10},
		{name: "piece length not power of two", r: strings.NewReader("x"), fileName: "f", pieceLength: 3 * pl},
		{name: "piece length above maximum", r: strings.NewReader("x"), fileName: "f", pieceLength: 8 << 20},
		{name: "read error", r: errReader{}, fileName: "f", pieceLength: pl},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Build(tt.r, tt.fileName, tt.pieceLength); err == nil {
				t.Error("Build succeeded, want error")
			}
		})
	}
}

// countingReader is an endless stream of zeros that records how much was read.
type countingReader struct{ n int64 }

func (c *countingReader) Read(p []byte) (int, error) {
	clear(p)
	c.n += int64(len(p))
	return len(p), nil
}

func TestBuild_StopsReadingPastLimit(t *testing.T) {
	t.Parallel()
	const limit = 4 * pl
	src := &countingReader{}
	if _, err := build(src, "f.bin", pl, limit); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want length-limit error", err)
	}
	if src.n > limit+pl {
		t.Errorf("read %d bytes from an endless source, want at most %d", src.n, limit+pl)
	}
}

func TestFromFile(t *testing.T) {
	t.Parallel()
	data := randomBytes(t, 40000)
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := FromFile(path, pl)
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if m.Info.Name != "movie.mkv" {
		t.Errorf("Name = %q, want base name movie.mkv", m.Info.Name)
	}
	if want := mustBuild(t, data, "movie.mkv"); m.InfoHash != want.InfoHash {
		t.Error("FromFile and Build disagree on info hash")
	}

	if _, err := FromFile(filepath.Join(t.TempDir(), "missing"), pl); err == nil {
		t.Error("FromFile on a missing file succeeded")
	}
}

func TestLoad_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, 2*int(pl) + 7} {
		orig := mustBuild(t, randomBytes(t, size), "f.bin")
		var buf bytes.Buffer
		if err := orig.Write(&buf); err != nil {
			t.Fatal(err)
		}
		got, err := Load(&buf, DefaultMaxLength)
		if err != nil {
			t.Fatalf("size %d: Load: %v", size, err)
		}
		if got.InfoHash != orig.InfoHash || got.NumPieces() != orig.NumPieces() || got.Info.Length != orig.Info.Length {
			t.Errorf("size %d: round trip changed the metainfo", size)
		}
	}
}

func TestLoad_NullPiecesForEmptyFile(t *testing.T) {
	t.Parallel()
	// "pieces": null and "pieces": [] describe the same empty file and must
	// produce the same info hash.
	empty := mustBuild(t, nil, "empty")
	doc := `{"version":1,"info":{"name":"empty","length":0,"piece_length":16384,"pieces":null},"info_hash":"` + empty.InfoHash + `"}`
	m, err := Load(strings.NewReader(doc), DefaultMaxLength)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.NumPieces() != 0 {
		t.Errorf("NumPieces = %d, want 0", m.NumPieces())
	}
}

func TestLoad_Rejects(t *testing.T) {
	t.Parallel()
	valid := *mustBuild(t, randomBytes(t, 2*int(pl)+5), "f.bin")

	// mutate returns a raw document derived from a copy of valid.
	mutate := func(rehash bool, f func(m *MetaInfo)) []byte {
		m := valid
		m.Info.Pieces = append([]string(nil), valid.Info.Pieces...)
		f(&m)
		return encodeRaw(t, m, rehash)
	}

	tests := []struct {
		name string
		doc  []byte
	}{
		{"malformed JSON", []byte(`{"version":1,`)},
		{"unknown field", []byte(`{"version":1,"evil":true}`)},
		{"trailing data", append(encodeRaw(t, valid, false), []byte(`{}`)...)},
		{"wrong version", mutate(false, func(m *MetaInfo) { m.Version = 2 })},
		{"tampered info hash", mutate(false, func(m *MetaInfo) { m.InfoHash = strings.Repeat("0", 64) })},
		{"tampered piece hash", mutate(false, func(m *MetaInfo) { m.Info.Pieces[0] = strings.Repeat("a", 64) })},
		{"tampered length", mutate(false, func(m *MetaInfo) { m.Info.Length-- })},
		{"too few piece hashes", mutate(true, func(m *MetaInfo) { m.Info.Pieces = m.Info.Pieces[:2] })},
		{"too many piece hashes", mutate(true, func(m *MetaInfo) { m.Info.Pieces = append(m.Info.Pieces, m.Info.Pieces[0]) })},
		{"negative length", mutate(true, func(m *MetaInfo) { m.Info.Length = -1 })},
		{"huge length", mutate(true, func(m *MetaInfo) { m.Info.Length = 1<<63 - 1 })},
		{"bad piece length", mutate(true, func(m *MetaInfo) { m.Info.PieceLength = 1000 })},
		{"uppercase hash", mutate(true, func(m *MetaInfo) { m.Info.Pieces[0] = strings.ToUpper(m.Info.Pieces[0]) })},
		{"short hash", mutate(true, func(m *MetaInfo) { m.Info.Pieces[0] = m.Info.Pieces[0][:62] })},
		{"non-hex hash", mutate(true, func(m *MetaInfo) { m.Info.Pieces[0] = strings.Repeat("zz", 32) })},
		{"parent dir name", mutate(true, func(m *MetaInfo) { m.Info.Name = ".." })},
		{"slash in name", mutate(true, func(m *MetaInfo) { m.Info.Name = "../../etc/passwd" })},
		{"backslash in name", mutate(true, func(m *MetaInfo) { m.Info.Name = `..\windows\win.ini` })},
		{"drive in name", mutate(true, func(m *MetaInfo) { m.Info.Name = "C:evil" })},
		{"control char in name", mutate(true, func(m *MetaInfo) { m.Info.Name = "a\x00b" })},
		{"overlong name", mutate(true, func(m *MetaInfo) { m.Info.Name = strings.Repeat("n", 256) })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load(bytes.NewReader(tt.doc), DefaultMaxLength); err == nil {
				t.Error("Load succeeded, want error")
			}
		})
	}
}

func TestLoad_MaxLength(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, randomBytes(t, 1000), "f.bin")
	var buf bytes.Buffer
	if err := m.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bytes.NewReader(buf.Bytes()), 999); err == nil {
		t.Error("Load accepted a file larger than maxLength")
	}
	if _, err := Load(bytes.NewReader(buf.Bytes()), 1000); err != nil {
		t.Errorf("Load rejected a file exactly at maxLength: %v", err)
	}
}

func TestLoad_OversizedDocument(t *testing.T) {
	t.Parallel()
	// Whitespace is valid JSON padding, so only the size cap can reject this.
	r := io.MultiReader(strings.NewReader(" "), io.LimitReader(spaceReader{}, maxMetaFileBytes))
	if _, err := Load(r, DefaultMaxLength); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want size-limit error", err)
	}
}

type spaceReader struct{}

func (spaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestLoadFile(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, randomBytes(t, 100), "f.bin")
	path := filepath.Join(t.TempDir(), "f.bin.meta.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := LoadFile(path, DefaultMaxLength)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.InfoHash != m.InfoHash {
		t.Error("LoadFile changed the info hash")
	}
}

func TestMetaInfo_PieceAccessors_OutOfRange(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, randomBytes(t, int(pl)+1), "f.bin")
	for _, i := range []int{-1, 2, 1 << 30} {
		if _, err := m.PieceHash(i); err == nil {
			t.Errorf("PieceHash(%d) succeeded", i)
		}
		if _, err := m.PieceSize(i); err == nil {
			t.Errorf("PieceSize(%d) succeeded", i)
		}
		if _, err := m.PieceOffset(i); err == nil {
			t.Errorf("PieceOffset(%d) succeeded", i)
		}
		if m.VerifyPiece(i, nil) {
			t.Errorf("VerifyPiece(%d) = true", i)
		}
	}
	if off, _ := m.PieceOffset(1); off != pl {
		t.Errorf("PieceOffset(1) = %d, want %d", off, pl)
	}
}

func TestMetaInfo_VerifyPiece_RejectsBadData(t *testing.T) {
	t.Parallel()
	data := randomBytes(t, int(pl)+10)
	m := mustBuild(t, data, "f.bin")

	corrupted := append([]byte(nil), data[:pl]...)
	corrupted[100] ^= 0xff
	tests := []struct {
		name  string
		index int
		data  []byte
	}{
		{"corrupted byte", 0, corrupted},
		{"truncated", 0, data[:pl-1]},
		{"padded last piece", 1, append(append([]byte(nil), data[pl:]...), 0)},
		{"other piece's data", 1, data[:pl]},
		{"nil", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if m.VerifyPiece(tt.index, tt.data) {
				t.Error("VerifyPiece = true, want false")
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	t.Parallel()
	valid := []string{"file.bin", ".hidden", "CONSOLE.txt", "nul_file", "COM10", "LPT.txt", "a b.mkv", "résumé.pdf"}
	invalid := []string{
		"", ".", "..", "a/b", `a\b`, "C:x", "a\x00b", "bad\xffutf8", strings.Repeat("n", 256),
		"NUL", "nul", "con.txt", "Com1.log", "LPT9", "aux .tar.gz", "trailing.", "trailing ",
	}
	for _, name := range valid {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range invalid {
		if err := validateName(name); err == nil {
			t.Errorf("validateName(%q) = nil, want error", name)
		}
	}
}

// FuzzLoad checks that arbitrary input never panics and that anything Load
// accepts has a self-consistent piece layout.
func FuzzLoad(f *testing.F) {
	for _, size := range []int{0, 1, int(pl) + 3} {
		var buf bytes.Buffer
		if err := mustBuild(f, randomBytes(f, size), "seed.bin").Write(&buf); err != nil {
			f.Fatal(err)
		}
		f.Add(buf.Bytes())
	}
	f.Add([]byte(`{"version":1,"info":{"name":"x","length":-5}}`))

	f.Fuzz(func(t *testing.T, doc []byte) {
		m, err := Load(bytes.NewReader(doc), DefaultMaxLength)
		if err != nil {
			return
		}
		var total int64
		for i := 0; i < m.NumPieces(); i++ {
			size, err := m.PieceSize(i)
			if err != nil || size <= 0 || size > m.Info.PieceLength {
				t.Fatalf("piece %d has size %d (err %v)", i, size, err)
			}
			total += size
		}
		if total != m.Info.Length {
			t.Fatalf("piece sizes sum to %d, want %d", total, m.Info.Length)
		}
	})
}
