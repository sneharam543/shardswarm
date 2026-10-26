# Tracker HTTP API

*Owner: Sneha Ram (Metainfo & Tracker), per the project proposal's work split.*

The tracker records which peers belong to which swarm and tells each peer about the others. It never sends or receives file content.

Implementation: `internal/tracker`. Run it with:

```
go run ./cmd/tracker [-addr HOST:PORT] [-interval DURATION]
```

The default address is `127.0.0.1:8080` (loopback only). Docker passes `-addr :8080` on its private network. The default interval is `15s`, and the minimum is `1s`.

All responses are JSON with `Content-Type: application/json`. Every error has the form `{"error": "<message>"}`.

## Identifiers

| Name | Format |
|------|--------|
| `info_hash` | 64 lowercase hex characters: the metainfo info hash (see METAINFO.md). It identifies the swarm. |
| `peer_id` | 40 lowercase hex characters: the 20-byte peer ID that is also sent in the wire handshake, hex-encoded. |

## `GET /announce`

This registers or refreshes a peer and returns other peers in the same swarm. A swarm is created on its first announce.

| Parameter | Required | Rule |
|-----------|----------|------|
| `info_hash` | yes | See above. |
| `peer_id` | yes | See above. |
| `port` | yes | The peer's wire-protocol listening port, 1–65535. |
| `left` | yes | Bytes still missing, ≥ 0. `0` means the peer is a seeder. |
| `event` | no | `started`, `completed`, `stopped`, or empty for a periodic update. |

The peer's IP is taken from the TCP connection, never from a parameter. Unknown parameters are ignored.

**200 OK**
```json
{"interval": 15, "peers": [{"peer_id": "…", "ip": "172.18.0.3", "port": 6881}]}
```
- `interval` is the number of seconds until the peer should announce again.
- `peers` holds up to 50 other live peers in random order. It never includes the caller. It is `[]` when there are none, and for `event=stopped`.

| Status | When |
|--------|------|
| 400 | A parameter is missing or invalid. |
| 409 | The `peer_id` is live in this swarm at a different IP or port. |
| 503 | The tracker already holds 1000 swarms, or this swarm already holds 1000 peers. |
| 405 | The method is not GET (or HEAD). |

`event=stopped` removes the peer only when it comes from the same IP and port that registered it. Knowing someone's `peer_id` is therefore not enough to remove them.

## `GET /scrape?info_hash=…`

**200 OK**
```json
{"complete": 2, "incomplete": 5}
```
`complete` counts live peers with `left = 0`. `incomplete` counts the other live peers.

| Status | When |
|--------|------|
| 400 | `info_hash` is missing or invalid. |
| 404 | The swarm does not exist. It was never announced, or all its peers have left or expired. |

## `GET /healthz`

Always returns `200 {"status":"ok"}`. Docker uses it as a health check.

## Expiry

- A peer is **live** until `2 × interval` passes without an announce from it. That is 30 s by default.
- Expired peers are left out of `/announce` and `/scrape` results immediately.
- A sweeper runs every `interval`. It deletes expired peers, and then any swarm that has no peers left.
- A `peer_id` held by an expired peer can be claimed from a new address.

## Limits and hardening

- Server timeouts: 5 s for headers, 10 s to read the request, 10 s to write the response, 60 s idle. Headers, including the query string, are capped at 8 KiB.
- The number of swarms, peers per swarm, and peers returned per response are all capped (1000, 1000 and 50).
- Peer IDs are not authenticated. Over plain HTTP, any host on the network can announce. This is acceptable on the private Docker network the project targets. It is **not** safe to expose this tracker to the internet.
- Per-IP rate limiting is not implemented. Add it before the tracker is exposed beyond a trusted network.
