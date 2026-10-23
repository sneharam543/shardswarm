// Owner: Sneha Ram (Metainfo & Tracker)

// Command mkmeta writes a metainfo file for a source file.
//
//	mkmeta [-piece-length N] [-out PATH] [-force] FILE
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sneharam543/shardswarm/internal/metainfo"
)

func main() {
	pieceLength := flag.Int64("piece-length", metainfo.DefaultPieceLength, "piece size in bytes (power of two, 16 KiB to 4 MiB)")
	out := flag.String("out", "", "output path (default FILE.meta.json)")
	force := flag.Bool("force", false, "overwrite an existing output file")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mkmeta [-piece-length N] [-out PATH] [-force] FILE")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *out, *pieceLength, *force); err != nil {
		fmt.Fprintln(os.Stderr, "mkmeta:", err)
		os.Exit(1)
	}
}

func run(src, out string, pieceLength int64, force bool) error {
	m, err := metainfo.FromFile(src, pieceLength)
	if err != nil {
		return err
	}
	if out == "" {
		out = src + ".meta.json"
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(out, flags, 0o644)
	if err != nil {
		return err // includes "file exists" without -force
	}
	if err := m.Write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	fmt.Printf("wrote %s\ninfo_hash %s\nlength %d bytes, %d pieces of %d bytes\n",
		out, m.InfoHash, m.Info.Length, m.NumPieces(), m.Info.PieceLength)
	return nil
}
