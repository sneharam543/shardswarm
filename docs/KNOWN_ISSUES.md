# Known issues and open items

For each item: what is wrong or unfinished, why it is not fixed in code, and what to do. All Go code passes `gofmt`, `go vet` and `go test -race ./...`; nothing on this list is a known bug in it.

## Optional: Docker

| # | Item | What to do |
|---|------|------------|
| 1 | **The Docker setup is optional and has not been run.** The demo and every experiment use local processes. `deploy/Dockerfile` and `docker-compose.yml` are written, `docker compose config` validates the file, and the Linux cross-build works. | With Docker running, check `docker run hello-world`, then run `docker compose up --build --scale leecher=5`. |

## Experiment findings that need a decision

| # | Finding | Evidence | Options |
|---|---------|----------|---------|
| 2 | **The proposal's claim is only half supported.** Rarest-first with tit-for-tat is faster than random or sequential with no choking (22.3 s vs 24.9 s and 80.1 s), but random with tit-for-tat is as fast (20.4 s). Tit-for-tat does not penalise the free-rider: in the 6-peer claim test it finished 2.0 s before the average contributor. | [docs/REPORT.md](REPORT.md#testing-the-proposals-claim), `experiments/results/claim/`, `experiments/results/freerider-study/README.md` | Reported as found. Untested hypothesis: in swarms of 6 to 8 peers, upload slots are not scarce. Next test: a larger swarm (e.g. 1 seeder, 12 contributors, 1 free-rider). |

## Deliberate limitations (documented, acceptable on a private network)

| # | Limitation | Why | When it would matter |
|---|-----------|-----|----------------------|
| 3 | No free-disk-space check before a download starts. A full disk shows up as a write error instead. | The Go standard library has no portable call for free space (noted in a comment in `internal/piece/store.go`). | Very large files on a nearly full disk. |
| 4 | The tracker has no per-IP rate limiting, no TLS and no authentication. Peers use plain TCP, and a `peer_id` is a claim, not an identity. | The project targets a trusted private Docker network (see `docs/TRACKER_API.md` and `docs/PROTOCOL.md` §8). | Any exposure to an untrusted network. |
| 5 | Connections that have not finished the handshake are limited only by the 10 s handshake timeout, not by a count. | Same as above (noted in a comment in `internal/peer/peer.go`). | A hostile host opening thousands of sockets. |
| 6 | The tracker returns at most 50 peers per announce. | This bounds response size. | Swarms larger than 51 peers may not become fully connected. Experiments use far fewer. |
| 7 | No endgame mode: the last piece waits for its one owner, or for the 30 s stall timeout. | A stretch goal in the spec. | Slow last pieces under tight rate limits (P8). |
| 8 | All engine state sits behind one lock, held during block reads and piece writes. | Simple and correct at tens of peers (noted in a comment in `internal/piece/engine.go`). | Hundreds of peers per process. |
| 9 | Only uploads are rate-limited; there is no separate download cap. | Every downloaded byte is another peer's capped upload, so the total is already bounded. | A peer downloading from uncapped peers, for example peers started without `-up-limit`. |

## Housekeeping

| # | Item | What to do |
|---|------|------------|
| 10 | The project is pushed as per-person PRs (one branch per member) in dependency order. | The Build progress table in the README shows which PRs have merged. |

## Fixed during the full-project check (for the record)

- **Finished peers kept their tit-for-tat upload slots until the next 10 s rechoke.** Waiting peers now get freed slots at once. The six-process test dropped from 10-12 s to about 5 s, and that remaining time is Windows starting six processes; each download inside it takes 30-130 ms.
- **A newcomer with the highest peer ID could wait up to one tracker interval (15 s) to be connected.** This happened because only the lower ID was allowed to dial. Now either side dials, and a fixed tie-break keeps the same connection on both sides.
- **A failed final rename left the peer reading from a closed file.** It now keeps seeding from the `.part` file and logs the error.
