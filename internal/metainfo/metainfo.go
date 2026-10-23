// Owner: Sneha Ram (Metainfo & Tracker)

// Package metainfo describes a shared file: its name, size, piece layout and
// the SHA-256 hash of every piece. The format is documented in docs/METAINFO.md.
package metainfo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// Version is the only metainfo format version this package accepts.
	Version = 1

	DefaultPieceLength int64 = 256 << 10 // 256 KiB
	MinPieceLength     int64 = 16 << 10  // one wire block
	MaxPieceLength     int64 = 4 << 20

	// DefaultMaxLength bounds the file size accepted by Load unless the
	// caller passes a different limit.
	DefaultMaxLength int64 = 4 << 30

	// maxMetaFileBytes caps how much JSON Load will read. 4 GiB at 16 KiB
	// pieces is 262144 hashes of ~67 bytes each, about 17.6 MB.
	maxMetaFileBytes = 32 << 20

	maxNameBytes = 255
)

// Info is the part of the metainfo covered by the info hash.
// Field order matters: the info hash is SHA-256 of json.Marshal(Info).
type Info struct {
	Name        string   `json:"name"`
	Length      int64    `json:"length"`
	PieceLength int64    `json:"piece_length"`
	Pieces      []string `json:"pieces"` // lowercase hex SHA-256, one per piece
}

// MetaInfo is the on-disk metainfo document.
type MetaInfo struct {
	Version  int    `json:"version"`
	Info     Info   `json:"info"`
	InfoHash string `json:"info_hash"` // lowercase hex SHA-256 of the canonical Info JSON

	pieceHashes [][sha256.Size]byte
	infoHash    [sha256.Size]byte
}

// Build reads r to EOF, splitting it into pieces of pieceLength bytes, and
// returns validated metainfo for a file called name.
func Build(r io.Reader, name string, pieceLength int64) (*MetaInfo, error) {
	return build(r, name, pieceLength, DefaultMaxLength)
}

func build(r io.Reader, name string, pieceLength, maxLength int64) (*MetaInfo, error) {
	// Read at most one byte past the limit so an oversized source fails
	// without being hashed to the end.
	r = io.LimitReader(r, maxLength+1)
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validatePieceLength(pieceLength); err != nil {
		return nil, err
	}

	info := Info{Name: name, PieceLength: pieceLength, Pieces: []string{}}
	buf := make([]byte, pieceLength)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			sum := sha256.Sum256(buf[:n])
			info.Pieces = append(info.Pieces, hex.EncodeToString(sum[:]))
			info.Length += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read source: %w", err)
		}
	}

	m := &MetaInfo{Version: Version, Info: info}
	h, err := hashInfo(info)
	if err != nil {
		return nil, err
	}
	m.InfoHash = hex.EncodeToString(h[:])
	if err := m.Validate(maxLength); err != nil {
		return nil, err
	}
	return m, nil
}

// FromFile builds metainfo for the file at path. The metainfo name is the
// file's base name.
func FromFile(path string, pieceLength int64) (*MetaInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > DefaultMaxLength {
		return nil, fmt.Errorf("length %d exceeds limit %d", fi.Size(), DefaultMaxLength)
	}
	return Build(f, filepath.Base(path), pieceLength)
}

// Load decodes and validates metainfo JSON. Files larger than maxLength bytes
// are rejected; pass DefaultMaxLength when there is no stricter limit.
func Load(r io.Reader, maxLength int64) (*MetaInfo, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMetaFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read metainfo: %w", err)
	}
	if len(data) > maxMetaFileBytes {
		return nil, fmt.Errorf("metainfo exceeds %d bytes", maxMetaFileBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m MetaInfo
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode metainfo: %w", err)
	}
	if dec.More() {
		return nil, errors.New("decode metainfo: trailing data after JSON object")
	}
	if err := m.Validate(maxLength); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadFile is Load for a file on disk.
func LoadFile(path string, maxLength int64) (*MetaInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f, maxLength)
}

// Write encodes m as indented JSON.
func (m *MetaInfo) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// Validate checks every field and caches the decoded hashes. It must succeed
// before any accessor is used; Build and Load call it for you.
func (m *MetaInfo) Validate(maxLength int64) error {
	if m.Version != Version {
		return fmt.Errorf("unsupported metainfo version %d", m.Version)
	}
	if err := validateName(m.Info.Name); err != nil {
		return err
	}
	if err := validatePieceLength(m.Info.PieceLength); err != nil {
		return err
	}
	// Bound the length before any arithmetic on it so the piece count cannot overflow.
	if m.Info.Length < 0 {
		return fmt.Errorf("negative length %d", m.Info.Length)
	}
	if m.Info.Length > maxLength {
		return fmt.Errorf("length %d exceeds limit %d", m.Info.Length, maxLength)
	}

	want := numPieces(m.Info.Length, m.Info.PieceLength)
	if int64(len(m.Info.Pieces)) != want {
		return fmt.Errorf("have %d piece hashes, want %d for length %d", len(m.Info.Pieces), want, m.Info.Length)
	}

	hashes := make([][sha256.Size]byte, len(m.Info.Pieces))
	for i, s := range m.Info.Pieces {
		if err := decodeHash(s, &hashes[i]); err != nil {
			return fmt.Errorf("piece %d: %w", i, err)
		}
	}

	var infoHash [sha256.Size]byte
	if err := decodeHash(m.InfoHash, &infoHash); err != nil {
		return fmt.Errorf("info_hash: %w", err)
	}
	got, err := hashInfo(m.Info)
	if err != nil {
		return err
	}
	if got != infoHash {
		return errors.New("info_hash does not match info")
	}

	m.pieceHashes = hashes
	m.infoHash = infoHash
	return nil
}

// InfoHashBytes returns the swarm identifier.
func (m *MetaInfo) InfoHashBytes() [sha256.Size]byte { return m.infoHash }

// NumPieces returns the number of pieces (0 for an empty file).
func (m *MetaInfo) NumPieces() int { return len(m.pieceHashes) }

// PieceHash returns the expected SHA-256 of piece i.
func (m *MetaInfo) PieceHash(i int) ([sha256.Size]byte, error) {
	if i < 0 || i >= len(m.pieceHashes) {
		return [sha256.Size]byte{}, fmt.Errorf("piece index %d out of range [0,%d)", i, len(m.pieceHashes))
	}
	return m.pieceHashes[i], nil
}

// PieceSize returns the size of piece i; only the last piece may be short.
func (m *MetaInfo) PieceSize(i int) (int64, error) {
	if i < 0 || i >= len(m.pieceHashes) {
		return 0, fmt.Errorf("piece index %d out of range [0,%d)", i, len(m.pieceHashes))
	}
	off := int64(i) * m.Info.PieceLength
	return min(m.Info.PieceLength, m.Info.Length-off), nil
}

// PieceOffset returns the byte offset of piece i within the file.
func (m *MetaInfo) PieceOffset(i int) (int64, error) {
	if i < 0 || i >= len(m.pieceHashes) {
		return 0, fmt.Errorf("piece index %d out of range [0,%d)", i, len(m.pieceHashes))
	}
	return int64(i) * m.Info.PieceLength, nil
}

// VerifyPiece reports whether data is exactly piece i.
func (m *MetaInfo) VerifyPiece(i int, data []byte) bool {
	want, err := m.PieceHash(i)
	if err != nil {
		return false
	}
	size, _ := m.PieceSize(i)
	return int64(len(data)) == size && sha256.Sum256(data) == want
}

func numPieces(length, pieceLength int64) int64 {
	if length == 0 {
		return 0
	}
	return (length-1)/pieceLength + 1
}

func hashInfo(info Info) ([sha256.Size]byte, error) {
	if info.Pieces == nil {
		info.Pieces = []string{} // "null" and "[]" must hash the same
	}
	b, err := json.Marshal(info)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode info: %w", err)
	}
	return sha256.Sum256(b), nil
}

// decodeHash accepts only canonical lowercase hex so each info has exactly
// one JSON encoding and therefore one info hash.
func decodeHash(s string, out *[sha256.Size]byte) error {
	if len(s) != 2*sha256.Size {
		return fmt.Errorf("hash must be %d hex characters, got %d", 2*sha256.Size, len(s))
	}
	if strings.ToLower(s) != s {
		return errors.New("hash must be lowercase hex")
	}
	if _, err := hex.Decode(out[:], []byte(s)); err != nil {
		return fmt.Errorf("invalid hex: %w", err)
	}
	return nil
}

// validateName rejects anything that could escape the download directory
// when joined to it. Names come from untrusted metainfo.
func validateName(name string) error {
	switch {
	case name == "":
		return errors.New("empty name")
	case len(name) > maxNameBytes:
		return fmt.Errorf("name longer than %d bytes", maxNameBytes)
	case name == "." || name == "..":
		return fmt.Errorf("invalid name %q", name)
	case strings.ContainsAny(name, `/\:`):
		return fmt.Errorf("name %q contains a path separator", name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return fmt.Errorf("name %q ends in a dot or space, which Windows strips", name)
	case isWindowsDevice(name):
		return fmt.Errorf("name %q is a reserved Windows device name", name)
	}
	for _, r := range name {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return fmt.Errorf("name %q contains an invalid or control character", name)
		}
	}
	return nil
}

// isWindowsDevice reports names like "NUL" or "com1.txt": on Windows, opening
// them reaches a device instead of creating a file.
func isWindowsDevice(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) &&
		stem[3] >= '0' && stem[3] <= '9'
}

func validatePieceLength(n int64) error {
	if n < MinPieceLength || n > MaxPieceLength || n&(n-1) != 0 {
		return fmt.Errorf("piece length %d must be a power of two in [%d, %d]", n, MinPieceLength, MaxPieceLength)
	}
	return nil
}
