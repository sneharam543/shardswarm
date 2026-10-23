# ShardSwarm implementation plan

> The original roadmap, written before the build and kept for reference. Current status is in [README.md](../README.md); results and conclusions are in `docs/REPORT.md` (arrives in PR #11).

P2P file distribution system (BitTorrent-lite) in Go with a custom tracker and a self-designed TCP peer wire protocol.
Repository: https://github.com/sneharam543/shardswarm

---

## 1. Goals and Non-Goals

**Goals**
- Split files into fixed-size pieces, verify each with SHA-256, and distribute them across a swarm of at least six concurrent peers (one or two seeders).
- Run our own HTTP tracker and our own documented binary TCP wire protocol.
- Provide rarest-first, random, and sequential piece selection, request pipelining, tit-for-tat choking with optimistic unchoke, and token-bucket rate limiting.
- Produce reproducible experiments (CSV → pandas → matplotlib) that measure swarm size, seeder count, selection strategy, and free-rider behaviour.

**Non-goals**
- No third-party BitTorrent libraries, databases, or external services.
- Go standard library only, unless a dependency is explicitly justified in a PR.
- No UDP tracker. HTTP is the chosen tracker transport.
- No TLS by default. The swarm runs on a trusted local Docker network; the limitation is documented in the report.

---

## 2. Phase 0 — Prerequisites

Go and Docker are not installed on the development machine yet (Python 3.14 and git are present).

```powershell
winget install GoLang.Go                 # Go >= 1.23
winget install Docker.DockerDesktop      # requires WSL2
python -m pip install pandas numpy matplotlib
```

Verify:
```powershell
go version
docker compose version
python -c "import pandas, numpy, matplotlib; print('ok')"
```

Initialize the module: `go mod init github.com/sneharam543/shardswarm`.

**Agent skills installed for this project:** golang-concurrency, golang-testing, golang-security, golang-error-handling, golang-code-style, docker-patterns, data-visualization, technical-writing. Also available locally: dataviz, code-review, security-review.

---

## 3. Repository Layout

```
cmd/
  tracker/        tracker server binary
  peer/           peer client binary (seeder / leecher / free-rider)
  mkmeta/         creates a .meta.json file from a source file
internal/
  metainfo/       Component A: piece hashing, (de)serialization, validation
  tracker/        Component B: swarm registry, announce/scrape handlers, expiry
  wire/           Component C: message encode/decode, framing, handshake
  peer/           Component D: connection manager, per-peer state machine
  piece/          Component E: picker strategies, piece store, verification, pipelining
  choke/          Component G: tit-for-tat + optimistic unchoke scheduler
  ratelimit/      Component H: token bucket, rate-limited net.Conn wrapper
  metrics/        Component J: CSV event recorder
  config/         flags + environment variable loading
docs/
  METAINFO.md  TRACKER_API.md  PROTOCOL.md  REPORT.md  VIVA.md
deploy/
  Dockerfile.tracker  Dockerfile.peer  docker-compose.yml  profiles/*.env
experiments/
  run_exp1.sh  run_exp2.sh  run_exp3.sh  analyze.py
  results/   (raw CSVs, committed)
  plots/     (generated PNGs)
testdata/
README.md  plan.md
```

---

## 4. Design Decisions (agree as a team before integration)

### 4.1 Metainfo (Component A)
- Format: JSON file `<name>.meta.json`.
  ```json
  {
    "version": 1,
    "info": {
      "name": "file.bin",
      "length": 20971520,
      "piece_length": 262144,
      "pieces": ["<hex sha256>", "..."]
    },
    "info_hash": "<hex sha256 of canonical info JSON>"
  }
  ```
- Default piece length: 256 KiB. Must be a power of two between 16 KiB and 4 MiB.
- Piece count = ceil(length / piece_length). The last piece size is `length - (n-1)*piece_length`.
- `info_hash` = SHA-256 of `json.Marshal(info)` (Go's deterministic struct field order). It identifies the swarm.
- Empty file: 0 pieces, valid. The leecher creates an empty file and completes immediately. This is tested.
- Validation on load: `version == 1`; name is non-empty, contains no path separators, and is not `.` or `..`; `length >= 0` and within the configured max (default 4 GiB); `len(pieces)` matches the computed count; every hash is 64 hex characters; `info_hash` matches the recomputed value.

### 4.2 Tracker (Component B) — implemented in P2, spec in docs/TRACKER_API.md
- `GET /announce?info_hash=&peer_id=&port=&left=&event=started|completed|stopped`
  - `info_hash` is 64 lowercase hex characters. `peer_id` is 40 lowercase hex characters (the 20-byte handshake ID). The peer's IP comes from the TCP connection.
  - 200 → `{"interval":15,"peers":[{"peer_id":"..","ip":"..","port":6881}]}`. The list excludes the caller and holds up to 50 peers in random order.
  - 400 for invalid parameters, 409 when a live `peer_id` announces from a different ip:port, 503 at capacity (1000 swarms, or 1000 peers per swarm), 405 for non-GET methods.
- `GET /scrape?info_hash=` → `{"complete":N,"incomplete":M}`. 404 for an unknown swarm.
- `GET /healthz` for Docker health checks.
- Swarms are created on the first announce and deleted once empty.
- Expiry: a peer is dead after 2 × interval (30 s) without an announce. Dead peers are hidden immediately. A sweeper runs every interval and stops through `context`.
- `event=stopped` is honoured only from the peer's own ip:port.
- State is guarded by a single `sync.Mutex`. The server sets header, read, write and idle timeouts, and caps headers at 8 KiB.
- Dropped as YAGNI: the `uploaded`/`downloaded` parameters (unused by the tracker), the `--meta-dir` allowlist, and per-IP 429 rate limiting (documented as required before any untrusted exposure).
- The tracker never transfers file content.

### 4.3 Wire Protocol (Component C) — implemented in P3, spec in docs/PROTOCOL.md
- Big-endian. 64-byte handshake: `11 | "SHARDSWARM1" | info_hash(32) | peer_id(20)`. It is rejected on a wrong protocol, a different swarm, or a self-connection.
- Frame: `length(4) | id(1) | payload`. `length = 0` is a KeepAlive. The maximum frame is `max(9 + 16 KiB, 1 + ceil(n/8))`, checked before allocation.
- IDs: Choke 0, Unchoke 1, Interested 2, NotInterested 3, Have 4, Bitfield 5, Request 6, Piece 7, Cancel 8.
- Blocks lie on a fixed 16 KiB grid. `begin` must be block-aligned and the length must be exactly the block's size, so a block is identified by `(index, begin)`.
- `wire.Read` validates against the metainfo. `wire.ErrInvalid` means the frame was consumed and the stream is still aligned (count a strike; close after 3). Any other error means close.
- P4 enforces the message order (Bitfield only first, Request only while unchoked, Piece only if requested) and the timeouts (dial 5 s, handshake 10 s, idle 120 s with a KeepAlive every 60 s, write 30 s, request 30 s).
- Deferred to P5 as YAGNI: a `Bitfield` helper type, whose first real user is the piece picker.

### 4.4 Choking Policy (Component G) — implemented in P7 (`internal/piece/choke.go`), committed: tit-for-tat + optimistic unchoke
- **Rechoke:** every 10 s, unchoke the top 3 *interested* peers. While leeching, peers are ranked by bytes received from them in the last round. While seeding, they are ranked by bytes sent to them. Ties are broken at random. All other peers are choked.
- **Optimistic unchoke:** one more interested peer, chosen at random from the choked ones. It is kept for 3 rounds (30 s), or replaced sooner if it loses interest or earns a regular slot. This is how newcomers and free-riders get data.
- **No idle first round:** an Interested peer is unchoked immediately while fewer than 4 *interested* peers are unchoked. When an unchoked peer sends NotInterested or disconnects, its slot is refilled from the waiting interested peers at once (`fillSlots`). The next rechoke reorders everyone by rate. (Fixed during the full-project check: finished peers used to hold their slots until the next 10 s rechoke.)
- **`-choke off`:** every interested peer is unchoked immediately and never re-choked. This is the baseline for Experiment 3.
- **`-choke freerider`:** the peer never unchokes anyone. P4 drops requests from choked peers, so it never uploads a byte (`Engine.Uploaded()` stays 0), even after it completes.
- **Rate window:** one 10 s round instead of the 20 s rolling window first planned. The counters reset every round, so no history is kept.
- **Stretch, not built:** endgame mode, super-seeding, anti-snubbing.

### 4.5 Rate Limiter (Component H) — implemented in P8 (`internal/ratelimit`, `peer.Manager.LimitUpload`)
- **Token bucket:** `rate` bytes/s. The bucket holds 20 ms worth of tokens, with a minimum of 16 KiB. That is large enough that timer resolution never caps throughput, and small enough that an idle peer cannot save up a burst. Refill is computed from elapsed time on each call, so there is no ticker goroutine. Standard library only.
- **`WaitN(ctx, n)`:** reserves the tokens straight away (the balance may go negative), then sleeps on one `time.Timer` for exactly the deficit. There is no busy-waiting, and concurrent writers queue in call order. If ctx is cancelled, the reservation is refunded.
- **Where it applies:** to real transfer writes. `peer.Conn`'s write loop sends every frame through `pacedWriter`, which writes in chunks of at most one bucket and waits for tokens before each chunk. Each chunk gets a fresh write deadline, so a low limit is never mistaken for a stalled peer. There is one limiter per peer process, shared by all its connections.
- **Config:** `peer -up-limit BYTES_PER_SEC` (0 = unlimited). `-debug` logs each throttled write.
- **Dropped as YAGNI:** a separate download limit (every downloaded byte is some peer's capped upload), per-connection limiters, unit suffixes, and environment variables. P9 Compose passes flags.
- **Measured:**
  - unit tests deliver 1 s of tokens in 1.00–1.09 s, with 1–4 writers, at up to 64 MiB/s;
  - a 300 KiB download from a seeder capped at 256 KiB/s took 1.16–1.18 s (theory: 1.11 s);
  - 1 seeder + 5 leechers, all capped at 1 MiB/s, with a 3 MB file: the leechers finished in 4.5–5.0 s, against 14.3 s if only the seeder uploaded and a 2.9 s floor.

### 4.6 Piece Store and Download Engine (Component E) — implemented in P5 (`internal/piece`)
- **Store:**
  - The download goes to `<dir>/<name>.part` (sparse `Truncate`, then `WriteAt` at piece offsets).
  - When every piece is in: fsync, close, then rename to `<name>`, refusing if `<name>` appeared meanwhile. The file is then reopened read-only for seeding.
  - An existing `<name>` is seeded only if every piece hash matches; it is never overwritten.
  - An existing `.part` resumes with whichever of its pieces pass their hash check.
- **Piece ownership:** each in-progress piece is owned by one peer, which supplies all of its blocks.
  - A failed hash check strikes exactly that peer, and the piece goes back to the pool.
  - On Choke, disconnect, or a 30 s stall, the partial piece is discarded and offered to the other peers.
  - A stalled owner gets a strike and is skipped for that round.
- **Picker:** one pass over the pieces handles every strategy. Rarest-first uses the lowest availability, sequential the lowest index, random a uniform choice; ties are broken uniformly by reservoir sampling. Availability is updated on Bitfield, Have and disconnect.
- **Pipelining:** up to `pipeline` (default 5) outstanding block requests per peer, refilled after every message.
- **Interest:** Interested / NotInterested is recomputed on Bitfield, Have and every verified piece.
- **Uploads:** answered as soon as the request arrives, and only for announced pieces. The placeholder policy, unchoke anyone interested, is replaced in P7.
- **Dropped as YAGNI:** the separate 16-request incoming cap (the send queue bounds requests), and the free-space check (no portable stdlib call; marked `ponytail:`).

### 4.6b Peer program (Component F) — implemented in P6 (`cmd/peer`, `tracker.Announce`)
- **Role:** decided by what is on disk (P5 store). There is no role flag. A leecher keeps seeding after it completes.
- **Announces:** `started` at launch, then one every interval from the tracker (or after 5 s if the announce failed), `completed` once, and `stopped` (best effort, 2 s) on SIGINT or SIGTERM. Every returned contact is passed to `Manager.Dial`, and the duplicate rule in P4 keeps one connection per peer.
- **Peer ID:** 20 random bytes from `crypto/rand` per run.
- **Tracker response:** not trusted. Entries with a bad peer ID, IP or port are skipped, and the body is capped at 1 MiB.
- **Integration test:** `TestSwarm_SixProcesses` re-executes the test binary as 6 real peer processes (1 seeder, 5 leechers) against a live tracker. It checks that every file is byte-identical and that the tracker's scrape reports 6 complete. `-short` skips it.
- **Deferred:** an "exit when done" flag and environment-variable configuration, until P9/P10 need them.

### 4.7 Concurrency Model (Component D) — implemented in P4 (`internal/peer`)
- Each connection has one reader goroutine and one writer goroutine. The writer drains a bounded queue of 256 messages. `Send` never blocks; if the queue is full, the slow peer is dropped.
- `Manager` accepts and dials connections. It enforces a 50-connection cap and keeps one connection per peer ID. Either side may dial. Of two connections to the same peer, the one dialled by the lower peer ID survives, on both sides. (Fixed during the full-project check: under the earlier "only the lower ID dials" rule, a newcomer with the highest ID could wait a whole tracker interval, up to 15 s, before anyone connected to it.)
- The choke and interest flags are atomics on `Conn`. A Bitfield after the first message counts as a strike, and 3 strikes close the connection. A Request arriving while we choke the peer is dropped with no strike.
- Events reach the engine through a 3-method `Handler` interface (Connected / Message / Disconnected), called from each connection's read goroutine. The engine (P5) guards its shared state with its own `sync.Mutex`.
- Cancelling the Manager's context closes every socket, including ones mid-handshake, through `context.AfterFunc`. `Wait()` returns when all goroutines have exited, and a test checks the goroutine count.
- Deferred: "Piece only if requested" and the 16-request queue cap go to P5 (the engine owns request state). Tracker announces go to P6. Upload/download statistics go to P7.

---

## 5. Milestones

Each milestone is one branch and one PR with tests. CI gate: `gofmt -l .` is empty, `go vet ./...`, `go test ./...`, `go test -race ./...`.

| #   | Milestone | Key output | Done when |
|-----|-----------|------------|-----------|
| P0  | Toolchain + scaffold | go.mod, dir layout, README skeleton, Makefile/`tasks.ps1` | `go build ./...` passes |
| P1  | Metainfo | `internal/metainfo`, `cmd/mkmeta`, docs/METAINFO.md | tests for empty/small/partial-last-piece/tampered meta |
| P2  | Tracker | `internal/tracker`, `cmd/tracker`, docs/TRACKER_API.md | httptest coverage: announce, scrape, expiry, dupes, invalid params, 100 concurrent announces with `-race` |
| P3  | Wire codec | `internal/wire`, docs/PROTOCOL.md | round-trip test for every message; truncated/oversized/unknown-ID tests; `FuzzReadMessage` |
| P4  | Peer connections | `internal/peer` conn + state machine | handshake success/failure over `net.Pipe`; timeout and disconnect tests |
| P5  | Download engine | `internal/piece` picker, store, verify, pipelining | corrupted piece rejected; file reconstructed byte-for-byte; strategy unit tests |
| P6  | Seeder/leecher | `cmd/peer` roles | integration test launching tracker + 1 seeder + 5 leechers as separate processes; all SHA-256 match |
| P7  | Choking | `internal/choke` | deterministic tests with fake clock; free-rider gets only optimistic slots |
| P8  | Rate limiter | `internal/ratelimit` | measured throughput within ±10% of limit over 5 s; no busy-wait |
| P9  | Metrics + Docker | `internal/metrics`, `deploy/` | `docker compose up` runs tracker + 2 seeders + 6 leechers; CSVs land in a mounted volume |
| P10 | Experiments | `experiments/`, plots | Exp 1–3 run ≥5 repeats each; analyze.py emits means ± std and charts |
| P11 | Docs + report | README, REPORT.md, VIVA.md | all §6 deliverables are checked off |
| S   | Stretch | endgame, super-seeding, DHT | only after P1–P11 are green |

Dependency order: P1 → (P2 ∥ P3) → P4 → P5 → P6 → (P7 ∥ P8) → P9 → P10 → P11.

---

## 6. Metrics CSV Schema (Component J)

One row per event (`piece_verified`, `block_recv`, `block_sent`, `complete`, `rate_sample`):

```
run_id,timestamp_ms,peer_id,event,piece_index,block_offset,bytes,rate_bps,complete,total_time_ms,strategy,choking,seeders,swarm_size,bw_limit_bps,free_rider
```

`rate_bps` is sampled every 1 s per peer. `total_time_ms` is filled on `complete` and is measured from the swarm start timestamp, which compose passes in as `RUN_START_MS`.

---

## 7. Experiments

Controlled settings for every experiment: file 20 MiB random bytes (fixed seed), piece 256 KiB, upload limit 1 MiB/s per peer, pipeline 5. Repeat each condition at least 5 times and report mean ± std.

1. **Swarm size × seeders.** Swarm sizes {4, 6, 8, 10} × seeders {1, 2}, strategy rarest-first, choking on. Metric: total distribution time, which is when the last leecher completes.
2. **Piece-selection strategy.** Fixed 1 seeder + 7 leechers. Compare rarest / random / sequential. Metrics: total time, per-peer download rate over time.
3. **Free-rider.** 1 seeder + 6 contributing leechers + 1 free-rider. Choking on vs off. Metrics: free-rider completion time vs mean contributor completion time, plus contributor download rates.

Plots: bar charts (time vs configuration, with error bars), line charts (rate vs time per peer), and a free-rider vs contributor comparison. Report only measured results. If a difference is within noise, say so.

---

## 8. Security Checklist (spec §4)

- [ ] All tracker query params validated: hex lengths, port 1–65535, numeric ranges, `event` enum. Request size capped.
- [ ] All wire fields validated against metainfo: `index < numPieces`; `begin + length ≤ pieceSize(index)` computed in uint64; `length ≤ 16 KiB`.
- [ ] Frame length checked before allocation. Bitfield spare bits must be zero.
- [ ] Filename from metainfo sanitized with `filepath.Base`, rejected if different. Output confined to `--download-dir`. Writes go to `.part` then rename, with no overwrite by default.
- [ ] Max file size and disk-space check before preallocation.
- [ ] Connection, read, write, and idle timeouts. Connection cap. Per-peer request queue cap. Strike counter leading to disconnect.
- [ ] Per-IP announce rate limit on tracker. Services bind to the Docker network. README warns against public exposure.
- [ ] Logs never include block contents. Peer IDs are logged truncated.
- [ ] Piece data is never trusted until its hash is verified. A peer's Have/Bitfield claims only affect availability, never completion.
- [ ] Documented limitation: SHA-256 gives integrity but not publisher authenticity. Signed metainfo is a possible extension.
- [ ] `go test -race ./...` passes. There is a goroutine-leak check in the integration tests.

---

## 9. Testing Matrix (spec §5)

| Area | Test |
|------|------|
| Metainfo | create/load round-trip, SHA-256 against known vectors, empty / 1-byte / exact-multiple / partial-last-piece, tampered info_hash rejected |
| Tracker | register + discover, duplicate IDs, expiry with an injected clock, concurrent announces (`-race`), invalid params, scrape counts |
| Wire | encode/decode every message, handshake ok / bad magic / wrong hash / self-connect, truncated, oversized, unknown ID, fuzz |
| Engine | request bounds validation, corrupted piece rejected + retried, reconstruction byte-for-byte, pipelining limit respected, Cancel handling |
| Concurrency | multi-peer transfer in-process and multi-process, peer disconnect mid-piece, stalled peer timeout |
| Choking | rechoke ranking, optimistic rotation, free-rider starvation vs choke-off |
| Rate limiter | throughput accuracy, burst cap, context cancel, no unbounded accumulation |
| Swarm | 6+ peer compose run, all outputs hash-equal to the source |
| Repro | same config + seed gives the same file/meta; experiment scripts rerunnable |

---

## 9b. P9 as built (metrics + Docker)
- **`internal/metrics`:** appends CSV rows (`piece`, `rate` every 1 s, `complete`) with the schema in the README. A nil recorder records nothing, so metrics can be switched off with no checks in callers. Repeated runs can append to one file, told apart by `run_id`.
- **`peer` reporter:** every second it takes one sample, which writes one CSV row and, when data moved, one `progress` log line. `Engine.Stats()` replaces `Left()`/`Uploaded()`, and `Config.OnPiece` records verified pieces.
- **`deploy/Dockerfile`:** one image (Go 1.23 build, Alpine 3.20 runtime, non-root user) containing all three programs.
- **`docker-compose.yml`:**
  - a one-shot `init` that uses `swarm-data/seed/$FILE` or creates a random file, then runs `mkmeta`;
  - a tracker with a `/healthz` health check, published on 127.0.0.1 only;
  - `seeder`, plus a `leecher` service whose count is set with `--scale`;
  - optional `seeder2` and `freerider` profiles.
  - All peers share one command template; each service sets only `PEER_DIR` and `PEER_CHOKE`.
- **Dropped as YAGNI:** per-block CSV rows (the 1 s rate samples and per-piece rows are enough for the experiments), a separate Dockerfile per program, and an exit-when-done flag. Experiments can watch the `complete` rows instead.

## 10. Risks and Open Questions

- **Windows host timing noise:** run experiments inside Docker on WSL2 and rely on the rate limiter so throughput is bounded by configuration, not host speed.
- **Docker Desktop resource limits:** 10 containers at 1 MiB/s is light, but check memory if piece length is raised.
- **Team agreement needed** on §4 formats before P4 integration. Changes go through a PR to `docs/`.
- **Clock for experiments:** use the container wall clock plus `RUN_START_MS` from the runner script. Containers share the host clock, so skew is negligible.

---

## 11. Definition of Done (spec §6 deliverables)

- [ ] 1. Go source for all components
- [ ] 2. docs/METAINFO.md
- [ ] 3. docs/TRACKER_API.md
- [ ] 4. docs/PROTOCOL.md
- [ ] 5. Seeder + leecher
- [ ] 6. Concurrent ≥6-peer swarm
- [ ] 7. Per-piece SHA-256 verification
- [ ] 8. Selection strategies + pipelining
- [ ] 9. Choking + free-rider experiment
- [ ] 10. Configurable rate limiting
- [ ] 11. Dockerfiles + Compose
- [ ] 12. Automated tests (incl. `-race`)
- [ ] 13. Experiment scripts, raw CSVs, plots
- [ ] 14. README (install, config, run, test, troubleshoot)
- [ ] 15. Technical report
- [ ] 16. Reproducible demo + viva material
