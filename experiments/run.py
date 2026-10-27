"""Run the ShardSwarm experiments as real local processes.
Owner: Swayam Kanjilal (Choking & Experiments)

Each run starts a fresh tracker, the seeders, the leechers (and a free-rider
where the experiment needs one), waits until every leecher has finished or
the timeout passes, then stops everything. Every peer writes its own metrics
CSV; one row per run goes to results/<experiment>/runs.csv.

    python experiments/run.py                      # all three, 5 repeats each
    python experiments/run.py exp3 --repeats 2     # one experiment
    python experiments/run.py --size-mb 4 --repeats 1   # quick smoke run

Standard library only. Needs Go on PATH to build the binaries.
"""

import argparse
import csv
import json
import os
import random
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
EXE = ".exe" if os.name == "nt" else ""

# Experiment definitions: each condition is one swarm configuration.
EXPERIMENTS = {
    # 1. Swarm size and seeder count (swarm size counts every peer).
    "exp1": [dict(swarm=n, seeders=s, strategy="rarest", choke="tft", freerider=False)
             for s in (1, 2) for n in (4, 6, 8, 10)],
    # 2. Piece-selection strategy: 1 seeder + 7 leechers.
    "exp2": [dict(swarm=8, seeders=1, strategy=st, choke="tft", freerider=False)
             for st in ("rarest", "random", "sequential")],
    # 3. Free-rider: 1 seeder + 6 contributors + 1 free-rider, choking on and off.
    "exp3": [dict(swarm=8, seeders=1, strategy="rarest", choke=ch, freerider=True)
             for ch in ("tft", "off")],
    # The proposal's claim (section 3): swarm fixed at 6 peers (1 seeder +
    # 4 contributors + 1 injected free-rider); strategy x choking varied together.
    "claim": [dict(swarm=6, seeders=1, strategy=st, choke=ch, freerider=True)
              for st in ("rarest", "random", "sequential") for ch in ("tft", "off")],
}

RUNS_HEADER = ["run_id", "experiment", "repeat", "swarm_size", "seeders", "leechers", "strategy",
               "choking", "freerider", "size_bytes", "up_limit_bps", "t0_ms",
               "distribution_ms", "completed", "timed_out"]


def build(bin_dir):
    bin_dir.mkdir(parents=True, exist_ok=True)
    subprocess.run(["go", "build", "-o", str(bin_dir) + os.sep, "./cmd/..."], cwd=ROOT, check=True)


def complete_ms(csv_path):
    """Timestamp of the peer's 'complete' row, or None."""
    try:
        with open(csv_path, newline="") as f:
            for row in csv.DictReader(f):
                if row["event"] == "complete":
                    return int(row["timestamp_ms"])
    except (FileNotFoundError, KeyError, ValueError):
        pass
    return None


def run_id_for(exp, cond, repeat):
    return f"{exp}-n{cond['swarm']}-s{cond['seeders']}-{cond['strategy']}-{cond['choke']}-r{repeat}"


def run_once(exp, cond, repeat, args, bin_dir, payload, meta, out_dir):
    leechers = cond["swarm"] - cond["seeders"]
    run_id = run_id_for(exp, cond, repeat)
    run_dir = out_dir / run_id
    if run_dir.exists():
        shutil.rmtree(run_dir)
    run_dir.mkdir(parents=True)
    work = Path(tempfile.mkdtemp(prefix="ss-run-"))
    procs, logs = [], []

    def start(name, binary, *flags):
        log = open(run_dir / f"{name}.log", "w")
        logs.append(log)
        procs.append(subprocess.Popen([str(bin_dir / (binary + EXE)), *flags], stdout=log, stderr=log))

    tracker = f"http://127.0.0.1:{args.tracker_port}"
    try:
        start("tracker", "tracker", "-addr", f"127.0.0.1:{args.tracker_port}", "-interval", "2s")
        time.sleep(0.5)
        if procs[0].poll() is not None:  # could not bind: peers would join a foreign swarm
            raise RuntimeError(f"tracker exited at start; see {run_dir / 'tracker.log'}")

        def peer(name, data_dir, choke):
            start(name, "peer", "-meta", str(meta), "-dir", str(data_dir), "-tracker", tracker,
                  "-listen", "127.0.0.1:0", "-up-limit", str(args.up_limit),
                  "-strategy", cond["strategy"], "-choke", choke,
                  "-metrics", str(run_dir / f"{name}.csv"), "-run-id", run_id,
                  "-seeders", str(cond["seeders"]), "-swarm-size", str(cond["swarm"]))

        for s in range(1, cond["seeders"] + 1):
            d = work / f"seeder{s}"
            d.mkdir()
            shutil.copy(payload, d / payload.name)
            peer(f"seeder{s}", d, cond["choke"])
        time.sleep(0.5)  # seeders are registered before any leecher asks

        t0 = int(time.time() * 1000)
        names = []
        for i in range(1, leechers + 1):
            free = cond["freerider"] and i == leechers
            name = "freerider" if free else f"leecher{i}"
            names.append(name)
            d = work / name
            d.mkdir()
            peer(name, d, "freerider" if free else cond["choke"])

        deadline = time.time() + args.timeout
        done = {}
        while len(done) < len(names) and time.time() < deadline:
            for n in names:
                if n not in done and (ms := complete_ms(run_dir / f"{n}.csv")) is not None:
                    done[n] = ms
            time.sleep(0.25)
    finally:
        for p in procs:
            p.terminate()
        for p in procs:
            p.wait()
        for log in logs:
            log.close()
        shutil.rmtree(work, ignore_errors=True)

    timed_out = len(done) < len(names)
    row = [run_id, exp, repeat, cond["swarm"], cond["seeders"], leechers, cond["strategy"],
           cond["choke"], cond["freerider"], payload.stat().st_size, args.up_limit, t0,
           "" if timed_out else max(done.values()) - t0, len(done), timed_out]
    status = "TIMED OUT" if timed_out else f"{(max(done.values()) - t0) / 1000:.1f}s"
    print(f"  {run_id}: {status} ({len(done)}/{len(names)} leechers)", flush=True)
    return row


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("experiments", nargs="*", default=list(EXPERIMENTS), choices=list(EXPERIMENTS))
    ap.add_argument("--repeats", type=int, default=5)
    ap.add_argument("--size-mb", type=int, default=16, help="shared file size in MiB")
    ap.add_argument("--up-limit", type=int, default=1 << 20, help="upload cap per peer in bytes/s")
    ap.add_argument("--timeout", type=int, default=240, help="seconds per run before giving up")
    ap.add_argument("--tracker-port", type=int, default=18200)
    ap.add_argument("--results", type=Path, default=ROOT / "experiments" / "results")
    args = ap.parse_args()

    # Two batches at once would share the tracker port and mix their swarms.
    with socket.socket() as s:
        try:
            s.bind(("127.0.0.1", args.tracker_port))
        except OSError:
            sys.exit(f"port {args.tracker_port} is in use: is another experiment batch running?")

    bin_dir = ROOT / "experiments" / "bin"
    build(bin_dir)

    # The same payload for every run, from a fixed seed, so runs are comparable.
    shared = Path(tempfile.mkdtemp(prefix="ss-payload-"))
    payload = shared / "payload.bin"
    rng = random.Random(42)
    payload.write_bytes(rng.randbytes(args.size_mb << 20))
    subprocess.run([str(bin_dir / ("mkmeta" + EXE)), str(payload)], check=True, stdout=subprocess.DEVNULL)
    meta = shared / "payload.bin.meta.json"

    for exp in args.experiments:
        out_dir = args.results / exp
        out_dir.mkdir(parents=True, exist_ok=True)
        print(f"{exp}: {len(EXPERIMENTS[exp])} conditions x {args.repeats} repeats", flush=True)
        # Each run is saved as soon as it ends, and runs already in runs.csv
        # are skipped, so an interrupted batch resumes where it stopped.
        runs_csv = out_dir / "runs.csv"
        done = set()
        if runs_csv.exists():
            with open(runs_csv, newline="") as f:
                done = {r["run_id"] for r in csv.DictReader(f)}
        with open(runs_csv, "a", newline="") as f:
            w = csv.writer(f)
            if not done:
                w.writerow(RUNS_HEADER)
            for cond in EXPERIMENTS[exp]:
                for r in range(1, args.repeats + 1):
                    if run_id_for(exp, cond, r) in done:
                        print(f"  {run_id_for(exp, cond, r)}: already recorded, skipped", flush=True)
                        continue
                    w.writerow(run_once(exp, cond, r, args, bin_dir, payload, meta, out_dir))
                    f.flush()
        (out_dir / "settings.json").write_text(json.dumps(
            {"repeats": args.repeats, "size_mb": args.size_mb, "up_limit_bps": args.up_limit,
             "tracker_interval": "2s", "piece_length": json.loads(meta.read_text())["info"]["piece_length"],
             "platform": sys.platform}, indent=2))
    shutil.rmtree(shared, ignore_errors=True)


if __name__ == "__main__":
    main()
