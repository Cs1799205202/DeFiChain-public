#!/usr/bin/env python3
"""Run a small experiment: several conditions x several seeds, in parallel, and tabulate.

    python3 pysdk/examples/sweep.py                 # the example below (feed vs spot oracle)
    python3 pysdk/examples/sweep.py --parallel 2    # fewer runs at once on a laptop

Edit CONDITIONS and SEEDS for your own question. Each run writes results/sweep/<cond>-s<seed>/
and the table goes to results/sweep/runs.csv. Runs use different ports, so they can run at
the same time; each one starts its own devnet (3 node processes) and uses a few CPU cores.
"""
import argparse
import concurrent.futures as cf
import csv
import json
import os
import statistics
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
OUT = os.path.join(ROOT, "results", "sweep")

COMMON = ["-duration", "70s", "-crash-at", "20s", "-crash-for", "15s", "-crash-pct", "0.25", "-borrowers", "6",
          "-ltv-min", "0.62", "-ltv-max", "0.74"]
CONDITIONS = {
    "feed": [],                                  # lending reads the oracle feed
    "spot": ["-spot-oracle", "-pool-weth", "150"],  # lending reads the AMM spot price, thin pool
}
SEEDS = [1, 2, 3]
METRICS = ["liquidations", "badDebtMax", "maxOracleLagPct", "reorgs", "height"]


def run(cond, seed, port):
    out = os.path.join(OUT, f"{cond}-s{seed}")
    os.makedirs(out, exist_ok=True)
    cmd = ["go", "run", "./cmd/sim", "-out", out, "-port", str(port), "-seed", str(seed)] + COMMON + CONDITIONS[cond]
    with open(os.path.join(out, "sim.log"), "w") as log:
        rc = subprocess.run(cmd, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT).returncode
    if rc != 0:
        return cond, seed, None
    return cond, seed, json.load(open(os.path.join(out, "summary.json")))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--parallel", type=int, default=3)
    a = ap.parse_args()
    jobs = [(c, s) for s in SEEDS for c in CONDITIONS]  # seeds outermost: conditions share conditions
    rows = []
    with cf.ThreadPoolExecutor(a.parallel) as ex:
        futs = [ex.submit(run, c, s, 30000 + 200 * i) for i, (c, s) in enumerate(jobs)]
        for f in cf.as_completed(futs):
            cond, seed, summ = f.result()
            if summ is None:
                print(f"{cond} seed {seed}: FAILED (see results/sweep/{cond}-s{seed}/sim.log)")
                continue
            rows.append({"condition": cond, "seed": seed, **{m: summ.get(m) for m in METRICS}})
            print(f"{cond} seed {seed}: " + ", ".join(f"{m}={summ.get(m)}" for m in METRICS), flush=True)
    os.makedirs(OUT, exist_ok=True)
    with open(os.path.join(OUT, "runs.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["condition", "seed"] + METRICS)
        w.writeheader()
        w.writerows(sorted(rows, key=lambda r: (r["condition"], r["seed"])))
    print("\nmean (sd) over seeds:")
    for cond in CONDITIONS:
        vals = [r for r in rows if r["condition"] == cond]
        if not vals:
            continue
        cells = []
        for m in METRICS:
            xs = [float(r[m]) for r in vals if r[m] is not None]
            sd = statistics.stdev(xs) if len(xs) > 1 else 0
            cells.append(f"{m} {statistics.mean(xs):.1f} ({sd:.1f})")
        print(f"  {cond:6s} " + "  ".join(cells))


if __name__ == "__main__":
    sys.exit(main())
