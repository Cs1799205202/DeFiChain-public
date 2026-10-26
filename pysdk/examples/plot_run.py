#!/usr/bin/env python3
"""Plot one simulation run: prices, lending health, and the agents' net worth.

    go run ./cmd/sim -out results/run1
    python3 pysdk/examples/plot_run.py results/run1          # writes results/run1/run.png

A starting point for your own figures: read the CSVs/JSON with the csv/json modules (or
pandas) and plot whatever your question needs.
"""
import csv
import json
import os
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402


def main(run):
    rows = list(csv.DictReader(open(os.path.join(run, "timeseries.csv"))))
    col = lambda k: [float(r[k]) for r in rows]
    t = col("t")
    summary = json.load(open(os.path.join(run, "summary.json")))
    chain = os.path.join(run, "chain_events.csv")  # every liquidation, whoever made it
    liq_t = [float(r["t"]) for r in csv.DictReader(open(chain)) if r["kind"] == "liquidate"] if os.path.exists(chain) else []

    fig, axes = plt.subplots(3, 1, figsize=(10, 10), sharex=True)
    ax = axes[0]
    ax.plot(t, col("truth"), label="exchange (real price)", color="black")
    ax.plot(t, col("oracle"), label="oracle feed", linestyle="--")
    ax.plot(t, col("pool"), label="AMM pool", alpha=0.8)
    if summary["config"].get("spotOracle"):
        ax.plot(t, col("lend_oracle"), label="lending price (AMM spot)", alpha=0.6)
    for x in liq_t:
        ax.axvline(x, color="red", alpha=0.3, linewidth=1)
    ax.set_ylabel("USD per WETH")
    ax.set_title(f"{run}: prices (red lines: liquidations)")
    ax.legend()

    ax = axes[1]
    ax.plot(t, col("debt"), label="total debt (USD)")
    ax.plot(t, col("bad_debt"), label="bad debt (USD)", color="red")
    ax2 = ax.twinx()
    ax2.step(t, col("unhealthy"), label="liquidatable positions", color="orange", where="post")
    ax2.step(t, col("reorgs"), label="reorgs so far", color="purple", where="post")
    ax.set_ylabel("USD")
    ax2.set_ylabel("count")
    ax.legend(loc="upper left")
    ax2.legend(loc="upper right")

    ax = axes[2]
    names = [a["name"] for a in summary["agents"]]
    pnl = [a["pnl"] for a in summary["agents"]]
    hold = [a["holdPnl"] for a in summary["agents"]]
    x = range(len(names))
    ax.bar([i - 0.2 for i in x], pnl, width=0.4, label="PnL")
    ax.bar([i + 0.2 for i in x], hold, width=0.4, label="buy-and-hold PnL", alpha=0.6)
    ax.set_xticks(list(x))
    ax.set_xticklabels(names, rotation=45, ha="right")
    ax.set_ylabel("USD (at the end price)")
    ax.axhline(0, color="gray", linewidth=0.8)
    ax.legend()
    ax.set_xlabel("")
    fig.tight_layout()
    out = os.path.join(run, "run.png")
    fig.savefig(out, dpi=120)
    print(f"wrote {out}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
