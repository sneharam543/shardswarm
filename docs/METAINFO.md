# Metainfo Format (version 1)

*Owner: Sneha Ram (Metainfo & Tracker), per the project proposal's work split.*

A metainfo file describes one shared file. It holds the file's name and size, how the file is split into pieces, and the SHA-256 hash of every piece. Peers need it before they can join a swarm. Create one with `mkmeta`:

```
go run ./cmd/mkmeta [-piece-length N] [-out PATH] [-force] FILE
```

The default output is `FILE.meta.json`. `mkmeta` refuses to overwrite an existing file unless `-force` is given.

Implementation: `internal/metainfo`.

## Example

```json
{
  "version": 1,
  "info": {
    "name": "sample.bin",
    "length": 700000,
    "piece_length": 262144,
    "pieces": [
      "<64 lowercase hex chars>",
      "<64 lowercase hex chars>",
      "<64 lowercase hex chars>"
    ]
  },
  "info_hash": "<64 lowercase hex chars>"
}
```

## Fields

| Field | Type | Rule |
|-------|------|------|
| `version` | int | Must be `1`. |
| `info.name` | string | 1–255 bytes. No `/`, `\` or `:`. Not `.` or `..`. No control characters or invalid UTF-8. Must not end in `.` or a space. Must not be a Windows device name (`CON`, `PRN`, `AUX`, `NUL`, `COM0`–`COM9`, `LPT0`–`LPT9`, in any case, with or without an extension). |
| `info.length` | int64 | File size in bytes. `0 ≤ length ≤ limit`. The default limit is 4 GiB; callers can lower it. |
| `info.piece_length` | int64 | A power of two from 16 KiB to 4 MiB. The default is 256 KiB. |
| `info.pieces` | []string | One lowercase hex SHA-256 per piece, in file order. |
| `info_hash` | string | Lowercase hex SHA-256 of the canonical `info` JSON. |

Unknown fields, trailing data after the JSON object, and documents larger than 32 MiB are rejected.

## Piece layout

- Piece count: `n = ceil(length / piece_length)`. An empty file has 0 pieces.
- Piece `i` starts at byte `i × piece_length`.
- Every piece is `piece_length` bytes except the last, which is `length − (n−1) × piece_length` bytes (between 1 and `piece_length`).

## Info hash

`info_hash = SHA-256(json.Marshal(info))`, where `json.Marshal` is Go's compact encoding with fields in the order `name, length, piece_length, pieces`. The info hash identifies the swarm. The tracker and the peer handshake both use it.

Each `info` has exactly one encoding, because hashes must be lowercase hex and Go encodes integers and strings deterministically. `"pieces": null` and `"pieces": []` both hash as `[]`.

## Empty files

A zero-length file is valid. It has 0 pieces and `"pieces": []`. A leecher creates the empty output file and is complete immediately, without contacting any peer.

## Validation on load

`metainfo.Load` checks the following, in order:

1. The version, name, and piece length.
2. The length bounds. These are checked before any arithmetic, so computing the piece count cannot overflow.
3. That the number of piece hashes matches the length.
4. That every hash is well-formed.
5. That `info_hash` matches the recomputed hash.

If any check fails, the whole file is rejected.

## Security notes

- `name` comes from an untrusted source. The validation rules above ensure that joining it to the download directory cannot escape that directory. On Windows, they also ensure the name cannot reach a device such as `NUL`.
- `mkmeta` rejects sources over the length limit before hashing them. `Build` stops reading one byte past the limit.
- Piece hashes protect against accidental corruption and against peers sending wrong data. They do **not** prove who published the file. Anyone who can replace the metainfo can also replace the content. Proving the publisher would need signed metainfo, which is out of scope for version 1.
