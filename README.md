# ShardSwarm

A small BitTorrent-style file sharing system: a file is split into pieces, and peers download them from each other instead of from one server. It is written in Go using only the standard library, with our own HTTP tracker and our own TCP peer protocol.

CS-30003 Coding Assignment 1, section CS-33, project P8. See the [project proposal](docs/P8_Project_Proposal.pdf) and the [roadmap](docs/PLAN.md).

## Team

Each source file names its owner in its header.

| Member | Area | Main folders |
|---|---|---|
| Sneha Ram (24155808) | Metainfo & Tracker | `internal/metainfo`, `internal/tracker`, `cmd/mkmeta`, `cmd/tracker` |
| Aditya Giri (24155747) | Peer Wire Protocol | `internal/wire`, `docs/PROTOCOL.md` |
| Rishav Sarkar (24155946) | Download Engine & Concurrency | `internal/peer`, `internal/piece` (engine, store), `cmd/peer` |
| Swayam Kanjilal (24155814) | Choking & Experiments | `internal/piece/choke.go`, `internal/ratelimit`, `internal/metrics`, `experiments/` |

## Project map

```
cmd/mkmeta/       makes a file's .meta.json (its "ID card": size, pieces, hashes)
cmd/tracker/      the tracker: tells peers who else has the file (HTTP)
cmd/peer/         the peer program: downloads and uploads pieces
internal/
  metainfo/       splits a file into 256 KiB pieces with SHA-256 hashes
  tracker/        tracker server and client
  wire/           peer-to-peer messages over TCP
  peer/           connections: handshake, timeouts, strikes for bad peers
  piece/          what to download next, hash checks, saving to disk, choking
  ratelimit/      upload speed limit (token bucket)
  metrics/        writes measurements to CSV
experiments/      run.py runs the experiments, analyze.py makes tables and plots
docs/             protocol, tracker API, metainfo format, report, known issues
deploy/           optional Docker setup (with docker-compose.yml)
```

## Build progress

Each PR changes its own row to ✅ Merged.

| PR | Owner | Adds | Status |
|---|---|---|---|
| Scaffold | Sneha | module, README, proposal, roadmap | ✅ Merged |
| #1 | Sneha | metainfo format, per-piece SHA-256, `mkmeta` | ✅ Merged |
| #2 | Swayam | upload rate limiter, CSV metrics | ✅ Merged |
| #3 | Sneha | tracker server and client | ✅ Merged |
| #4 | Aditya | wire protocol and its specification | ✅ Merged |
| #5 | Rishav | peer connections | ✅ Merged |
| #6 | Rishav + Swayam | download engine, store, choking | ✅ Merged |
| #7 | Rishav | the `peer` program and the 6-process test | ✅ Merged |
| #8 | Swayam | experiments, results and plots | ✅ Merged |
| #9 | Sneha | report, known issues | ⏳ Planned |
| #10 | Sneha | Docker setup (optional) | ⏳ Planned |

## Requirements

Go 1.23 or later. For the experiments: Python 3 with pandas, numpy and matplotlib.

## How to run it

Everything runs on one PC as ordinary programs: one `tracker` and one `peer` per participant, each on its own port.

**One terminal per program:**

```
go build -o bin/ ./cmd/...
bin/mkmeta seed/movie.bin                                                  # writes seed/movie.bin.meta.json
bin/tracker -addr 127.0.0.1:8080
bin/peer -meta seed/movie.bin.meta.json -dir seed -listen 127.0.0.1:6881   # seeder: the file is already in -dir
bin/peer -meta seed/movie.bin.meta.json -dir dl1  -listen 127.0.0.1:6882   # leecher: empty -dir
bin/peer -meta seed/movie.bin.meta.json -dir dl2  -listen 127.0.0.1:6883
```

A peer is a seeder or a leecher depending only on what is in its `-dir`. If it finds a half-finished `.part` file there, it resumes it.

**Several PCs:** run the tracker with `-addr 0.0.0.0:8080` and give each peer `-tracker http://<tracker-ip>:8080 -listen 0.0.0.0:6881`. Copy the same `.meta.json` to every PC and allow TCP 8080 and 6881 through the firewall. There is no encryption, so use this on a trusted network only.

**Docker (optional):** `docker compose up --build --scale leecher=5`. The settings are documented in `docker-compose.yml`.

| `peer` flag | Default | Meaning |
|---|---|---|
| `-meta` | (required) | the `.meta.json` file |
| `-dir` | `.` | download folder |
| `-tracker` | `http://127.0.0.1:8080` | tracker address |
| `-listen` | `127.0.0.1:6881` | this peer's address |
| `-strategy` | `rarest` | piece order: `rarest`, `random` or `sequential` |
| `-choke` | `tft` | upload policy: `tft` (tit-for-tat), `off` (upload to everyone) or `freerider` (never upload) |
| `-up-limit` | `0` | upload cap in bytes/s (`1048576` = 1 MiB/s; `0` = no limit) |
| `-pipeline` | `5` | requests kept in flight per peer |
| `-metrics` | off | append measurements to this CSV (columns listed in `internal/metrics/metrics.go`) |
| `-debug` | off | extra logging |

## Experiments

```
python experiments/run.py                    # all experiments, 5 repeats each (about 50 min)
python experiments/run.py exp3 --repeats 2   # just one
python experiments/analyze.py                # writes results/summary.md and plots/
```

Every run uses the same 16 MiB file, a 1 MiB/s upload cap per peer and a fresh tracker. Times are seconds until the last leecher finishes, averaged over 5 runs.

| Experiment | What changes | Result |
|---|---|---|
| claim | piece strategy × choking, with 1 free-rider | rarest + tit-for-tat 22.3 s vs sequential 64.6 s. Choking speeds things up, but the free-rider was not slowed down. |
| exp1 | swarm size 4–10, 1 or 2 seeders | 1 seeder: 19.2 → 21.4 s; 2 seeders: 9.6 → 15.0 s |
| exp2 | rarest / random / sequential | 21.2 / 22.0 / 65.1 s |
| exp3 | tit-for-tat on / off, with 1 free-rider | contributors 20.8 s with tit-for-tat vs 28.5 s without |

Full tables: `experiments/results/summary.md`. Plots: `experiments/plots/`.

## Testing

```
go vet ./...
go test -race ./...    # every package, with the race detector
go test -short ./...   # skips the 6-process swarm test
```
