"""Summarise experiment results and draw the charts.
Owner: Swayam Kanjilal (Choking & Experiments)

Reads results/<exp>/runs.csv (one row per run, written by run.py) and the
per-peer metrics CSVs next to it. Writes results/summary.md and PNG charts
in experiments/plots/. Every number comes from the recorded measurements.

    python experiments/analyze.py [--results DIR] [--plots DIR]
"""

import argparse
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

ROOT = Path(__file__).resolve().parent
RESULTS, PLOTS = ROOT / "results", ROOT / "plots"  # overridable with --results / --plots

# Reference palette, first three categorical slots: the only ones that pass
# all-pairs colour-blind checks together. Text never uses series colours.
BLUE, ORANGE, AQUA = "#2a78d6", "#eb6834", "#1baf7a"
SURFACE, INK, INK2, GRID = "#fcfcfb", "#0b0b0b", "#52514e", "#e4e3df"

plt.rcParams.update({
    "figure.facecolor": SURFACE, "axes.facecolor": SURFACE, "savefig.facecolor": SURFACE,
    "axes.edgecolor": GRID, "axes.labelcolor": INK2, "text.color": INK,
    "xtick.color": INK2, "ytick.color": INK2, "axes.spines.top": False, "axes.spines.right": False,
    "axes.grid": True, "axes.grid.axis": "y", "grid.color": GRID, "grid.linewidth": 0.8,
    "axes.axisbelow": True, "font.size": 10, "axes.titlesize": 11, "legend.frameon": False,
})


def runs(exp):
    df = pd.read_csv(RESULTS / exp / "runs.csv")
    df["seconds"] = df["distribution_ms"] / 1000
    return df


def peer_rows(exp, run_id):
    frames = []
    for f in (RESULTS / exp / run_id).glob("*.csv"):
        d = pd.read_csv(f)
        d["peer"] = f.stem
        frames.append(d)
    return pd.concat(frames, ignore_index=True)


def stats(series):
    s = series.dropna()
    return dict(n=len(s), mean=s.mean(), std=s.std(ddof=1) if len(s) > 1 else 0.0, min=s.min(), max=s.max())


def bars(ax, groups, series, values, errors, colors, labels):
    """Grouped bars with standard-deviation whiskers and a value label on every bar."""
    x = np.arange(len(groups))
    width = 0.8 / len(series)
    for i, (name, color) in enumerate(zip(series, colors)):
        pos = x - 0.4 + width * (i + 0.5)
        ax.bar(pos, values[i], width, color=color, edgecolor=SURFACE, linewidth=2, label=labels[i], zorder=2)
        ax.errorbar(pos, values[i], yerr=errors[i], fmt="none", ecolor=INK2, elinewidth=1, capsize=3, zorder=3)
        for p, v, e in zip(pos, values[i], errors[i]):
            if not np.isnan(v):
                ax.text(p, v + (0 if np.isnan(e) else e) + 0.3, f"{v:.1f}", ha="center", va="bottom", fontsize=8, color=INK2)
    ax.set_xticks(x, groups)
    top = np.nanmax(np.array(values, dtype=float) + np.nan_to_num(np.array(errors, dtype=float)))
    ax.set_ylim(0, top * 1.22)  # headroom so the legend never covers a bar or its label


def table(rows, header):
    out = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    out += ["| " + " | ".join(str(c) for c in r) + " |" for r in rows]
    return "\n".join(out)


def fmt(s):
    return [s["n"], f"{s['mean']:.2f}", f"{s['std']:.2f}", f"{s['min']:.2f}", f"{s['max']:.2f}"]


def exp1(md):
    df = runs("exp1")
    sizes, seeders = sorted(df.swarm_size.unique()), sorted(df.seeders.unique())
    rows, means, errs = [], [], []
    for s in seeders:
        m, e = [], []
        for n in sizes:
            st = stats(df[(df.seeders == s) & (df.swarm_size == n)].seconds)
            m.append(st["mean"]), e.append(st["std"])
            timeouts = int(df[(df.seeders == s) & (df.swarm_size == n)].timed_out.sum())
            rows.append([n, s, n - s] + fmt(st) + [timeouts])
        means.append(m), errs.append(e)
    fig, ax = plt.subplots(figsize=(7, 4))
    bars(ax, [str(n) for n in sizes], seeders, means, errs, [BLUE, ORANGE], [f"{s} seeder" + ("s" if s > 1 else "") for s in seeders])
    ax.set(xlabel="Swarm size (peers, including seeders)", ylabel="Distribution time (s)",
           title="Exp 1: time until every leecher has the file")
    ax.legend(loc="upper left", ncol=2)
    fig.tight_layout(), fig.savefig(PLOTS / "exp1_swarm_size_seeders.png", dpi=150), plt.close(fig)
    md.append("## Experiment 1: swarm size and seeder count\n")
    md.append(table(rows, ["swarm", "seeders", "leechers", "runs", "mean s", "std s", "min s", "max s", "timeouts"]))


def rate_curve(exp, run_ids, t0s, names_filter=None):
    """Mean over runs of the swarm's total leecher download rate, per second since t0."""
    curves = []
    for run_id, t0 in zip(run_ids, t0s):
        d = peer_rows(exp, run_id)
        d = d[(d.event == "rate") & ~d.peer.str.startswith("seeder")]
        if names_filter:
            d = d[d.peer.map(names_filter)]
        d = d.assign(sec=((d.timestamp_ms - t0) // 1000).astype(int))
        curves.append(d.groupby("sec").down_bps.sum() / (1 << 20))
    return pd.concat(curves, axis=1).fillna(0).mean(axis=1).sort_index()


def exp2(md):
    df = runs("exp2")
    order = ["rarest", "random", "sequential"]
    colors = dict(zip(order, [BLUE, ORANGE, AQUA]))
    rows, means, errs = [], [], []
    for st in order:
        s = stats(df[df.strategy == st].seconds)
        means.append(s["mean"]), errs.append(s["std"])
        rows.append([st] + fmt(s) + [int(df[df.strategy == st].timed_out.sum())])
    fig, ax = plt.subplots(figsize=(6, 4))
    bars(ax, order, ["all"], [means], [errs], [BLUE], ["mean ± std over runs"])
    ax.set(xlabel="Piece-selection strategy", ylabel="Distribution time (s)",
           title="Exp 2: distribution time by strategy (1 seeder, 7 leechers)")
    fig.tight_layout(), fig.savefig(PLOTS / "exp2_strategy_time.png", dpi=150), plt.close(fig)

    fig, ax = plt.subplots(figsize=(7, 4))
    for st in order:
        sub = df[(df.strategy == st) & ~df.timed_out]
        curve = rate_curve("exp2", sub.run_id, sub.t0_ms)
        ax.plot(curve.index, curve.values, color=colors[st], linewidth=2, label=st)
        ax.annotate(st, (curve.index[-1], curve.values[-1]), xytext=(4, 0), textcoords="offset points",
                    va="center", fontsize=8, color=INK2)
    ax.set(xlabel="Seconds since leechers started", ylabel="Total leecher download rate (MiB/s)",
           title="Exp 2: swarm download rate over time (mean of runs)")
    ax.legend(loc="upper right")
    fig.tight_layout(), fig.savefig(PLOTS / "exp2_rate_over_time.png", dpi=150), plt.close(fig)
    md.append("\n## Experiment 2: piece-selection strategy\n")
    md.append(table(rows, ["strategy", "runs", "mean s", "std s", "min s", "max s", "timeouts"]))


def exp3(md):
    df = runs("exp3")
    order = ["tft", "off"]
    contrib, free, rows = {c: [] for c in order}, {c: [] for c in order}, []
    for _, r in df.iterrows():
        d = peer_rows("exp3", r.run_id)
        done = d[d.event == "complete"].set_index("peer").timestamp_ms - r.t0_ms
        c = done[[p for p in done.index if p.startswith("leecher")]] / 1000
        contrib[r.choking].append(c.mean() if len(c) else np.nan)
        free[r.choking].append(done.get("freerider", np.nan) / 1000)
    means = [[stats(pd.Series(contrib[c]))["mean"] for c in order], [stats(pd.Series(free[c]))["mean"] for c in order]]
    errs = [[stats(pd.Series(contrib[c]))["std"] for c in order], [stats(pd.Series(free[c]))["std"] for c in order]]
    for c in order:
        sc, sf = stats(pd.Series(contrib[c])), stats(pd.Series(free[c]))
        rows.append([c, "contributors (mean of 6)"] + fmt(sc))
        rows.append([c, "free-rider"] + fmt(sf))
    fig, ax = plt.subplots(figsize=(6, 4))
    bars(ax, ["tit-for-tat", "choking off"], ["c", "f"], means, errs, [BLUE, ORANGE],
         ["contributors (mean of 6)", "free-rider"])
    ax.set(xlabel="Choking policy of the contributing peers", ylabel="Completion time (s)",
           title="Exp 3: free-rider vs contributors")
    ax.legend(loc="upper left", ncol=2)
    fig.tight_layout(), fig.savefig(PLOTS / "exp3_freerider.png", dpi=150), plt.close(fig)
    md.append("\n## Experiment 3: free-rider\n")
    md.append(table(rows, ["choking", "peers", "runs", "mean s", "std s", "min s", "max s"]))


def claim(md):
    """The proposal's claim: 6 peers, strategy x choking, with a free-rider."""
    df = runs("claim")
    strategies, chokes = ["rarest", "random", "sequential"], ["tft", "off"]
    contrib = {(s, c): [] for s in strategies for c in chokes}       # last contributor: distribution time
    contrib_avg = {(s, c): [] for s in strategies for c in chokes}   # average contributor: fair free-rider baseline
    free = {(s, c): [] for s in strategies for c in chokes}
    rates = {(s, c): [] for s in strategies for c in chokes}  # per-run per-peer rate frames
    for _, r in df[~df.timed_out].iterrows():
        d = peer_rows("claim", r.run_id)
        done = d[d.event == "complete"].set_index("peer").timestamp_ms - r.t0_ms
        key = (r.strategy, r.choking)
        leechers = done[[p for p in done.index if p.startswith("leecher")]] / 1000
        contrib[key].append(leechers.max())
        contrib_avg[key].append(leechers.mean())
        free[key].append(done.get("freerider", np.nan) / 1000)
        rr = d[(d.event == "rate") & ~d.peer.str.startswith("seeder")]
        rates[key].append(rr.assign(sec=((rr.timestamp_ms - r.t0_ms) // 1000).astype(int)))

    rows = []
    for s in strategies:
        for c in chokes:
            sc, sa, sf = (stats(pd.Series(x[(s, c)])) for x in (contrib, contrib_avg, free))
            rows.append([s, c, sc["n"], f"{sc['mean']:.2f} ± {sc['std']:.2f}", f"{sa['mean']:.2f} ± {sa['std']:.2f}",
                         f"{sf['mean']:.2f} ± {sf['std']:.2f}", f"{sf['mean'] - sa['mean']:+.2f}"])
    labels = {"tft": "tit-for-tat", "off": "choking off"}
    for name, data, title in [("claim_distribution_time.png", contrib, "Contributors: distribution time"),
                              ("claim_freerider_time.png", free, "Free-rider: completion time")]:
        means = [[stats(pd.Series(data[(s, c)]))["mean"] for s in strategies] for c in chokes]
        errs = [[stats(pd.Series(data[(s, c)]))["std"] for s in strategies] for c in chokes]
        fig, ax = plt.subplots(figsize=(7, 4))
        bars(ax, strategies, chokes, means, errs, [BLUE, ORANGE], [labels[c] for c in chokes])
        ax.set(xlabel="Piece-selection strategy", ylabel="Time (s)", title=f"Claim test (6 peers): {title}")
        ax.legend(loc="upper left", ncol=2)
        fig.tight_layout(), fig.savefig(PLOTS / name, dpi=150), plt.close(fig)

    # Per-peer download rate: mean over runs, one line per peer; contributors
    # share one colour (one identity), the free-rider has its own.
    fig, axes = plt.subplots(2, 3, figsize=(11, 6), sharex=True, sharey=True)
    for i, c in enumerate(chokes):
        for j, s in enumerate(strategies):
            ax = axes[i][j]
            if not rates[(s, c)]:
                continue
            per = pd.concat(rates[(s, c)]).groupby(["peer", "sec"]).down_bps.mean() / (1 << 20)
            for peer in sorted(per.index.get_level_values(0).unique()):
                line = per.loc[peer]
                fr = peer == "freerider"
                ax.plot(line.index, line.values, color=ORANGE if fr else BLUE,
                        linewidth=2 if fr else 1, alpha=1 if fr else 0.6,
                        label=("free-rider" if fr else "contributor") if j == 0 and i == 0 and (fr or peer == "leecher1") else None)
            ax.set_title(f"{s}, {labels[c]}", fontsize=9)
            if j == 0:
                ax.set_ylabel("Download rate (MiB/s)")
            if i == 1:
                ax.set_xlabel("Seconds since leechers started")
    axes[0][0].legend(loc="upper right", fontsize=8)
    fig.suptitle("Claim test: per-peer download rate over time (mean of runs)")
    fig.tight_layout(), fig.savefig(PLOTS / "claim_per_peer_rate.png", dpi=150), plt.close(fig)

    md.append("\n## Claim test: 6 peers, strategy x choking, with a free-rider\n")
    md.append("Distribution time = the last of the 4 contributors finishing. The free-rider is compared with the "
              "average contributor (a single peer against a single peer); positive = the free-rider is slower.\n")
    md.append(table(rows, ["strategy", "choking", "runs", "distribution s (last contributor)", "average contributor s",
                           "free-rider s", "free-rider minus average contributor s"]))


def main():
    global RESULTS, PLOTS
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--results", type=Path, default=RESULTS)
    ap.add_argument("--plots", type=Path, default=PLOTS)
    args = ap.parse_args()
    RESULTS, PLOTS = args.results, args.plots
    PLOTS.mkdir(parents=True, exist_ok=True)
    md = ["# Experiment results\n",
          "All times are seconds from the moment the leechers started (runner `t0`). Mean and sample standard deviation are taken over repeated runs. Raw data: `results/<exp>/runs.csv` and the per-peer CSVs.\n"]
    for name, fn in [("claim", claim), ("exp1", exp1), ("exp2", exp2), ("exp3", exp3)]:
        if (RESULTS / name / "runs.csv").exists():
            fn(md)
    (RESULTS / "summary.md").write_text("\n".join(md) + "\n", encoding="utf-8")
    print("\n".join(md))


if __name__ == "__main__":
    main()
