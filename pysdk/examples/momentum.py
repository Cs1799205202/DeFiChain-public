#!/usr/bin/env python3
"""A rule-based agent: trend following on the exchange price (Part 2 baseline example).

    go run ./cmd/sim -duration 90s -agent "mom=python3 pysdk/examples/momentum.py"

Every few seconds: if the short moving average of the price is above the long one by more
than a threshold, buy WETH with part of our USD; if it is below, sell part of our WETH.
Simple, transparent, and a fair baseline to compare an LLM agent against.
"""
import os
import sys
from collections import deque

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from defichain import World  # noqa: E402

SHORT, LONG = 3, 10        # moving-average windows, in decisions
THRESHOLD = 0.004          # 0.4% gap between the averages before we act
FRACTION = 0.25            # trade a quarter of the position each time

prices = deque(maxlen=LONG)


def step(w: World):
    p = w.prices()
    ref = p["exchange"] or p["oracle"]
    prices.append(ref)
    if len(prices) < LONG:
        w.log_decision([], f"warming up ({len(prices)}/{LONG} prices)")
        return
    short = sum(list(prices)[-SHORT:]) / SHORT
    long_ = sum(prices) / LONG
    signal = short / long_ - 1
    h = w.holdings()
    actions, reason = [], f"signal {signal:+.4f}"
    if signal > THRESHOLD and h["usd"] > 100:
        usd = h["usd"] * FRACTION
        w.buy_weth(usd)
        actions = [{"action": "buy_weth", "amount_usd": round(usd, 2)}]
        reason += ": uptrend, buy"
    elif signal < -THRESHOLD and h["weth"] > 0.01:
        weth = h["weth"] * FRACTION
        w.sell_weth(weth)
        actions = [{"action": "sell_weth", "amount_weth": round(weth, 4)}]
        reason += ": downtrend, sell"
    w.log_decision(actions, reason, price=ref, pool=p["pool"])
    print(f"[{w.name}] block {p['block']} price {ref:.1f} pool {p['pool']:.1f} {reason}", flush=True)


if __name__ == "__main__":
    w = World.from_env()
    w.approve_all()
    print(f"[{w.name}] {w.address} starting with {w.holdings()}", flush=True)
    w.run(step, every=3)
    print(f"[{w.name}] done, net worth {w.net_worth():.0f}", flush=True)
