# ShardSwarm Peer Wire Protocol (version 1)

*Owner: Aditya Giri (Peer Wire Protocol), per the project proposal's work split.*

Peers exchange file data directly over TCP using this protocol. The tracker only introduces them to each other (see TRACKER_API.md).

Implementation: `internal/wire` (encoding, decoding and per-message validation). Connection state, message order and timeouts are in `internal/peer`.

## Conventions

- All integers are unsigned and **big-endian**.
- A piece is a unit from the metainfo (default 256 KiB). A **block** is a 16 KiB slice of a piece and is the unit of transfer.
- `n` is the number of pieces in the metainfo.

## 1. Handshake

Each side sends exactly 64 bytes as soon as the TCP connection opens. Both sides write first and then read. The 64 bytes fit in any TCP send buffer, so this cannot deadlock.

| Offset | Size | Field | Value |
|--------|------|-------|-------|
| 0 | 1 | `pstrlen` | `11` |
| 1 | 11 | `pstr` | ASCII `SHARDSWARM1` |
| 12 | 32 | `info_hash` | Raw SHA-256 info hash of the swarm |
| 44 | 20 | `peer_id` | Raw sender peer ID (the tracker shows it as 40 hex characters) |

The receiver closes the connection if:
- `pstrlen` or `pstr` differ (another protocol or version);
- `info_hash` differs from its own (a different swarm);
- `peer_id` equals its own (it has connected to itself).

## 2. Framing

After the handshake, every message is one frame:

```
length (4 bytes) | id (1 byte) | payload (length − 1 bytes)
```

- `length` counts the id and the payload, but not the length field itself.
- `length = 0` is a **KeepAlive**. It has no id and no payload.
- TCP is a byte stream. A receiver reads exactly 4 bytes, then exactly `length` bytes (`io.ReadFull`).
- **Maximum frame length:** `max(1 + 8 + 16384, 1 + ceil(n / 8))`. That is 16393 bytes, unless the bitfield is larger: a 4 GiB file in 16 KiB pieces has a 32769-byte bitfield frame. The receiver checks `length` against this limit **before** allocating, and closes the connection if it is larger.

## 3. Messages

| id | Name | Payload | `length` |
|----|------|---------|----------|
| – | KeepAlive | none | 0 |
| 0 | Choke | none | 1 |
| 1 | Unchoke | none | 1 |
| 2 | Interested | none | 1 |
| 3 | NotInterested | none | 1 |
| 4 | Have | `index` u32 | 5 |
| 5 | Bitfield | `ceil(n / 8)` bytes | 1 + ceil(n / 8) |
| 6 | Request | `index` u32, `begin` u32, `length` u32 | 13 |
| 7 | Piece | `index` u32, `begin` u32, block bytes | 9 + block size |
| 8 | Cancel | `index` u32, `begin` u32, `length` u32 | 13 |

Example: a request for piece 1, second block, is
`00 00 00 0d | 06 | 00 00 00 01 | 00 00 40 00 | 00 00 40 00`.

### Meaning

- **Choke / Unchoke:** the sender will not, or will, serve the receiver's requests. Every connection starts *choked*.
- **Interested / NotInterested:** whether the sender wants pieces the receiver has. Every connection starts *not interested*.
- **Have:** the sender has just verified piece `index`.
- **Bitfield:** which pieces the sender has. Bit 7 (the most significant bit) of byte 0 is piece 0. A set bit means the sender has that piece.
- **Request:** asks for one block.
- **Piece:** one block of data, answering a request.
- **Cancel:** withdraws an earlier request with the same `index`, `begin` and `length`.

### Validity rules (enforced by `wire.Read`)

| Rule | Applies to |
|------|-----------|
| Payload size exactly as in the table | All ids |
| `index < n` | Have, Request, Piece, Cancel |
| Exactly `ceil(n / 8)` bytes, and unused trailing bits are 0 | Bitfield |
| `begin` is a multiple of 16384 and `begin < pieceSize(index)` | Request, Piece, Cancel |
| Block size `= min(16384, pieceSize(index) − begin)` | Request and Cancel (`length`), Piece (block bytes) |

All arithmetic on peer-supplied values is done in 64 bits, so `begin + length` cannot overflow.

Blocks always lie on a fixed 16 KiB grid. Partial or oversized blocks are invalid, which means a block is identified by `(index, begin)` alone.

## 4. Errors

| Kind | Examples | Action |
|------|----------|--------|
| **Invalid message** (`wire.ErrInvalid`) | Unknown id, wrong payload size, index out of range, off-grid block, bad bitfield | The frame was fully read and the stream is still aligned. Count a strike, ignore the message, and keep reading. After **3 strikes**, close the connection. |
| **Fatal** | Frame over the length limit, truncated frame, I/O error, timeout | Close the connection. The stream can no longer be trusted. |

A malformed message never crashes the peer. This is checked by table tests and a fuzz test (`FuzzRead`).

## 5. Message order

1. Handshake, in both directions.
2. Optionally, a **Bitfield** as the very first message. A peer with no pieces may omit it. A Bitfield anywhere else counts as an invalid message.
3. After that, any message in any order, subject to:
   - **Request** is served only while the sender has unchoked the requester. A Request received while choking is dropped silently, **without** a strike: an honest peer may have sent it before our Choke reached it. (`internal/peer`)
   - **Piece** is accepted only if it matches an outstanding Request `(index, begin)` to that peer. An unexpected or late Piece is dropped silently, since it may answer a request we have already released. (`internal/piece`)
   - On **Choke**, the receiver treats all of its outstanding requests to that peer as cancelled.
   - A **Request** for a piece the receiver has not announced counts as a strike. A leecher never serves unverified bytes from its partial file.
   - Requests are answered as soon as they arrive, so a **Cancel** has nothing left to withdraw and is ignored. Requests are bounded by the receiver's 256-message send queue: a peer that floods requests faster than it reads the replies is disconnected (§7).
   - A Bitfield that is not the first message after the handshake counts as a strike. KeepAlives do not count as a first message.

Data from a Piece message counts only after the whole piece has been verified against its SHA-256 hash (see METAINFO.md). A peer's Have and Bitfield claims only affect which peers we ask for a piece. They never mark a piece complete.

## 6. Timeouts

| Phase | Timeout |
|-------|---------|
| TCP dial | 5 s |
| Handshake (write and read) | 10 s |
| Read with no traffic | 120 s. Each side sends a KeepAlive after 60 s without sending. |
| Single write to the socket | 30 s. Under an upload limit, each chunk of a frame gets its own 30 s deadline, so slow pacing does not count as a stalled peer. |
| Piece in progress | If no block of the piece arrives for 30 s, its outstanding blocks are cancelled, the peer gets a strike, and the piece goes back to the pool. On that tick, the stalled peer is not offered new pieces. |

## 7. Connections (`internal/peer`)

- **One connection per pair of peers.** Either peer may dial. If two connections to the same peer exist (both dialled at once, or a redial), the one **dialled by the numerically lower `peer_id`** is kept and the other is closed. Both sides apply the same rule, so they keep the same TCP connection. When dialling, the handshake must also return the `peer_id` the tracker advertised.
- **At most 50 connections per peer.** Beyond that, new sockets are closed before the handshake. A second connection from a `peer_id` that is already connected is closed after the handshake.
- **Sending never blocks the sender.** Each connection has a 256-message send queue. If it fills up, the peer is not reading fast enough, and the connection is closed.
- **Shutdown** closes every socket, including ones still mid-handshake, and waits for all connection goroutines to exit.

## 8. Security notes

- Every length, index and offset from a peer is checked against the trusted metainfo before it is used.
- The protocol has no encryption and no peer authentication. A `peer_id` is a claim, not an identity. This is acceptable on the private Docker network the project targets, but not on untrusted networks.
- Integrity comes from the per-piece SHA-256 hashes, not from the transport.
