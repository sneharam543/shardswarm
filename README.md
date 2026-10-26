# ShardSwarm

A peer-to-peer file distribution system inspired by BitTorrent. It is written in Go with a custom HTTP tracker and a custom TCP peer wire protocol. See [docs/PLAN.md](docs/PLAN.md) for the original roadmap.

## Project map

```
shardswarm/                               (PR #n) = arrives with that PR; see Build progress
├── README.md               you are here: how to run everything
├── go.mod                  Go module (standard library only)
├── cmd/                    the three programs you run
│   ├── mkmeta/             makes a file's .meta.json "ID card"            Sneha    (PR #1)
│   ├── tracker/            the tracker (peer phone book, HTTP)            Sneha    (PR #3)
│   └── peer/               the seeder/leecher program + 6-process test    Rishav   (PR #7)
├── internal/               the building blocks the programs use
│   ├── metainfo/           pieces + SHA-256 fingerprints                  Sneha    (PR #1)
│   ├── tracker/            tracker server + client                        Sneha    (PR #3)
│   ├── wire/               the peer-to-peer message format (TCP)          Aditya   (PR #4)
│   ├── peer/               connections: handshake, strikes, timeouts      Rishav   (PR #5)
│   ├── piece/              engine.go + store.go: download engine          Rishav   (PR #6)
│   │                       choke.go: tit-for-tat choking                  Swayam   (PR #6)
│   ├── ratelimit/          upload speed limiter (token bucket)            Swayam   (PR #2)
│   └── metrics/            CSV measurements                               Swayam   (PR #2)
├── experiments/            run.py, analyze.py, results/, plots/           Swayam   (PR #8)
├── docs/                   PLAN, proposal PDF; METAINFO (#1), TRACKER_API (#3),
│                           PROTOCOL (#4), REPORT + KNOWN_ISSUES (#11), VIVA (#9)
├── obsidian/               visual notes + canvases (Obsidian vault)       Rishav   (PR #10)
├── scripts/demo.ps1        one-command local demo                         Rishav   (PR #10)
├── deploy/, docker-compose.yml, .dockerignore   optional Docker            Sneha    (PR #12)
└── .gitignore, .gitattributes
```

**Start here, by person.** Read `obsidian/Start Here.md` first (PR #10), then:
- **Sneha:** `internal/metainfo`, `internal/tracker`, `docs/METAINFO.md`, `docs/TRACKER_API.md`.
- **Aditya:** `internal/wire`, `docs/PROTOCOL.md`.
- **Rishav:** `internal/peer`, `internal/piece/engine.go` and `store.go`, `cmd/peer`.
- **Swayam:** `internal/piece/choke.go`, `internal/ratelimit`, `internal/metrics`, `experiments/`.

Everyone: `docs/REPORT.md` for the full story (PR #11), and `docs/VIVA.md` for the questions on your area (PR #9).

## Build progress

The project is built in PRs, one area per teammate, in the order below; each PR only depends on PRs above it. **Each PR changes its own row to ✅ Merged.**

| PR | Owner | Adds | Status |
|---|---|---|---|
| Scaffold | Sneha | module, this README, proposal, roadmap, ignore rules | ✅ Merged |
| #1 | Sneha | metainfo format, per-piece SHA-256, `mkmeta` | ✅ Merged |
| #2 | Swayam | upload rate limiter, CSV metrics | ✅ Merged |
| #3 | Sneha | tracker server and client | ✅ Merged |
| #4 | Aditya | wire protocol and its specification | ⏳ Planned |
| #5 | Rishav | peer connections (handshake, strikes, timeouts) | ⏳ Planned |
| #6 | Rishav + Swayam | download engine and store, choking (joint PR) | ⏳ Planned |
| #7 | Rishav | the `peer` program and the 6-process test | ⏳ Planned |
| #8 | Swayam | experiments, results and plots | ⏳ Planned |
| #11 | Sneha | technical report, known issues | ⏳ Planned |
| #9 | Aditya | viva preparation | ⏳ Planned |
| #10 | Rishav | demo script, Obsidian notes | ⏳ Planned |
| #12 | Sneha | Docker setup (optional) | ⏳ Planned |

Known issues, open items and limitations: `docs/KNOWN_ISSUES.md` (arrives in PR #11).

## Browse in Obsidian

The repo doubles as an [Obsidian](https://obsidian.md) vault for explaining the project. In Obsidian, choose **Open folder as vault**, pick this folder, then open `obsidian/Start Here.md`.

- **`obsidian/Start Here.md`:** a 10-minute guided explanation.
- **`obsidian/A Download Step by Step.md`:** a sequence diagram of one download.
- **`obsidian/concepts/`:** one short note per idea. The graph view (Ctrl+G) shows how they connect.
- **Canvases:** `Architecture`, `Experiments` (all plots) and `Project Status`.

## Requirements

Go 1.23 or later. Python 3 with pandas, numpy and matplotlib for the experiments. Docker only for the optional container setup.

## Team

CS-30003 Coding Assignment 1, section CS-33, project P8. The work split follows the [project proposal](docs/P8_Project_Proposal.pdf), and each source file names its owner in its header.

| Member | Area |
|---|---|
| Sneha Ram (24155808) | Metainfo & Tracker: `internal/metainfo`, `internal/tracker`, `cmd/mkmeta`, `cmd/tracker` |
| Aditya Giri (24155747) | Peer Wire Protocol: `internal/wire`, `docs/PROTOCOL.md` |
| Rishav Sarkar (24155946) | Download Engine & Concurrency: `internal/piece` (engine, store), `internal/peer`, `cmd/peer` |
| Swayam Kanjilal (24155814) | Choking & Experiments: `internal/piece/choke.go`, `internal/ratelimit`, `internal/metrics`, `experiments/` |

Integration, the report and viva preparation are shared by all four.

## How to run it

You do **not** need separate servers. Every part is an ordinary program: one `tracker`, plus one `peer` per participant. They can all run on one PC, because each peer listens on its own port.

### A. One command, one PC (quickest)

```powershell
powershell -ExecutionPolicy Bypass -File scripts\demo.ps1
```

This builds everything, starts a tracker, 1 seeder and 5 leechers as separate processes, shares a random 3 MB file at 1 MiB/s per peer, checks every copy's SHA-256, and stops everything. Options:

```powershell
scripts\demo.ps1 -Leechers 7 -SizeMB 10 -UpLimit 2097152 -Strategy random -Choke off
```

Logs are written to `%TEMP%\shardswarm-demo`.

### B. By hand, one PC (to watch each part)

Open one terminal per program. The steps are in **Usage** below. Give each peer its own `-dir` and its own `-listen` port.

### C. Several PCs on the same network

| Machine | Command |
|---------|---------|
| Tracker PC (e.g. `192.168.1.10`) | `tracker -addr 0.0.0.0:8080` |
| Seeder PC | `peer -meta file.bin.meta.json -dir seed -tracker http://192.168.1.10:8080 -listen 0.0.0.0:6881` |
| Each leecher PC | `peer -meta file.bin.meta.json -dir dl -tracker http://192.168.1.10:8080 -listen 0.0.0.0:6881` |

- Copy the same `.meta.json` file to every machine. Only the seeder needs the real file.
- Allow TCP 8080 (tracker) and 6881 (peers) through Windows Firewall on those machines.
- Use this only on a trusted network. There is no encryption or authentication (see `docs/KNOWN_ISSUES.md`, PR #11).

### D. Docker Compose (optional)

Runs every peer in its own container on one PC. Nothing else depends on it; the demo and the experiments use local processes. It needs Docker running; see `docs/KNOWN_ISSUES.md` item 1 (PR #11).

```powershell
docker compose up --build --scale leecher=5                        # tracker + 1 seeder + 5 leechers
docker compose --profile freerider up --build --scale leecher=5    # plus 1 free-rider
$env:SEEDERS=2; docker compose --profile seeder2 up --build --scale leecher=4
docker compose down
```

- **Your own file:** put it in `swarm-data\seed\` and set `$env:FILE="yourfile.ext"` before `up`. Without one, a random file of `SIZE_MB` megabytes (default 20) is created there.
- **Other settings:** `UP_LIMIT` (bytes/s per peer, default 1048576), `STRATEGY`, `CHOKE`, `INTERVAL` (tracker, default 5s), and `RUN_ID`, `SEEDERS`, `SWARM_SIZE` (labels for the metrics only).
- **Outputs:** one CSV per container in `swarm-data\results\`. The tracker is published at `http://127.0.0.1:8080` on this PC only.

## Usage

```
go build -o bin/ ./cmd/...

# 1. Describe the file to share (format: docs/METAINFO.md)
bin/mkmeta seed/movie.bin                          # writes seed/movie.bin.meta.json

# 2. Start the tracker (API: docs/TRACKER_API.md)
bin/tracker -addr 127.0.0.1:8080

# 3. Seeder: the complete file is already in -dir
bin/peer -meta seed/movie.bin.meta.json -dir seed -listen 127.0.0.1:6881

# 4. Leechers: empty -dir; each downloads, then keeps seeding
bin/peer -meta seed/movie.bin.meta.json -dir dl1 -listen 127.0.0.1:6882
bin/peer -meta seed/movie.bin.meta.json -dir dl2 -listen 127.0.0.1:6883 -strategy random
```

A peer is a seeder or a leecher depending only on what is already in `-dir`:
- **Seeder:** a complete, hash-verified file is there.
- **Resuming leecher:** a `.part` file is there. Pieces in it that pass their hash check are kept.
- **New leecher:** the directory is empty.

| `peer` flag | Default | Meaning |
|-------------|---------|---------|
| `-meta` | (required) | Metainfo file |
| `-dir` | `.` | Download directory |
| `-tracker` | `http://127.0.0.1:8080` | Tracker base URL |
| `-listen` | `127.0.0.1:6881` | Address for incoming peer connections (`:0` means any free port) |
| `-strategy` | `rarest` | `rarest`, `random` or `sequential` |
| `-pipeline` | `5` | Outstanding block requests per peer |
| `-choke` | `tft` | Upload policy. `tft` is tit-for-tat with optimistic unchoke. `off` unchokes every interested peer. `freerider` never uploads. |
| `-up-limit` | `0` | Upload cap in bytes per second, shared by all connections (`1048576` = 1 MiB/s). `0` means unlimited. |
| `-metrics` | off | Append measurements to this CSV file (schema below) |
| `-run-id`, `-seeders`, `-swarm-size` | | Labels written into the metrics rows |
| `-debug` | off | Log debug events: strikes, rate limiting, rejected handshakes. |

## Metrics (CSV)

Run `peer -metrics FILE.csv` to append one peer's measurements; the experiment runner and the optional Docker setup do this automatically. Each peer also prints a `progress` line every second while data is moving. Every row has every column; a value that does not apply to the event is empty.

| Column | Meaning |
|--------|---------|
| `run_id` | Label for the run (`-run-id`; default: the peer's start time) |
| `timestamp_ms` | Unix time in milliseconds |
| `peer_id` | First 4 bytes of the peer ID, in hex |
| `event` | `piece` (a verified piece), `rate` (sampled every second), or `complete` (whole file verified) |
| `piece_index`, `piece_bytes` | For `piece` events |
| `bytes_down`, `bytes_up` | Cumulative bytes transferred (`rate`) |
| `down_bps`, `up_bps` | Bytes per second over the last sample (`rate`) |
| `pieces_have`, `pieces_total` | Progress (`rate`) |
| `complete` | `true` once the file is complete (`rate`, `complete`) |
| `total_time_ms` | From peer start to completion (`complete`) |
| `strategy`, `choking`, `bw_limit_bps`, `free_rider` | This peer's configuration |
| `seeders`, `swarm_size` | Experiment labels (`-seeders`, `-swarm-size`) |

## Experiments

```
python experiments/run.py                          # every experiment, 5 repeats each (about 50 min)
python experiments/run.py exp3 --repeats 2         # one experiment
python experiments/run.py --size-mb 4 --repeats 1  # quick smoke run
python experiments/analyze.py                      # writes results/summary.md and plots/*.png
```

`run.py` builds the binaries and starts every peer as a separate local process, with a fresh tracker for each run. No Docker is needed.

- **Controlled for every run:** the same 16 MiB payload (fixed seed), a 1 MiB/s upload cap on every peer, 256 KiB pieces, and a 2 s tracker interval.
- **What each experiment varies:**

| Experiment | Varies | Fixed |
|------------|--------|-------|
| claim | the proposal's claim: strategy rarest/random/sequential × choking tft/off | 6 peers: 1 seeder + 4 contributors + 1 free-rider |
| exp1 | swarm size 4/6/8/10 × seeders 1/2 | rarest-first, tit-for-tat |
| exp2 | strategy: rarest / random / sequential | 1 seeder + 7 leechers, tit-for-tat |
| exp3 | choking: tit-for-tat / off | 1 seeder + 6 contributors + 1 free-rider, rarest-first |

- **Outputs:**
  - `experiments/results/<exp>/runs.csv`: one row per run, including the distribution time, which is the time from leecher start until the last leecher completes;
  - `experiments/results/<exp>/<run_id>/`: every peer's metrics CSV and log;
  - `experiments/results/summary.md` and `experiments/plots/`.

## Testing

```
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
go test -short ./...   # skips the six-process swarm test
```
