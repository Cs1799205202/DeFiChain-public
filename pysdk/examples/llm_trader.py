#!/usr/bin/env python3
"""An LLM trading agent: the starting point for Part 2. Copy it and make it yours.

    go run ./cmd/mockllm &                    # offline stand-in, no API key needed
    go run ./cmd/sim -duration 120s -agent "llm=python3 pysdk/examples/llm_trader.py --every 8"

With a real model (any OpenAI-compatible API):
    export LLM_BASE_URL=https://api.deepseek.com/v1 LLM_MODEL=deepseek-chat LLM_API_KEY_FILE=~/.llm-key
    python3 pysdk/examples/llm_check.py       # check the setup and wake the model up first

The loop is observe -> prompt -> decide -> validate -> act -> remember:
  * observe:  prices (pool, oracle, exchange), our holdings, the lending position;
  * prompt:   a system prompt with the rules of the market and the output format, and a user
              prompt with the observation and our recent history;
  * decide:   the model answers with a JSON plan of at most MAX_ACTIONS actions;
  * validate: "the model proposes, the code disposes": unknown actions are dropped,
              amounts are clamped to what we actually have, and we never borrow to the limit;
  * act:      one transaction per action, stop at the first failure;
  * remember: the outcome goes into the history shown in the next prompt.
"""
import argparse
import json
import math
import os
import sys
import time
from collections import deque

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from defichain import LLM, Reverted, World  # noqa: E402

MAX_ACTIONS = 3
DEFAULT_PERSONA = "You are a careful trader: preserve capital first, keep your health factor above 1.5."
# Command line (python3 llm_trader.py --every 10 --persona "..."), or environment variables.
_ap = argparse.ArgumentParser(description="LLM trading agent (run it from cmd/sim -agent)")
_ap.add_argument("--every", type=float, default=float(os.environ.get("AGENT_EVERY", "8")), help="seconds between decisions")
_ap.add_argument("--persona", default=os.environ.get("AGENT_PERSONA", DEFAULT_PERSONA), help="appended to the system prompt")
ARGS = _ap.parse_args()
EVERY, PERSONA = ARGS.every, ARGS.persona

SYSTEM = """You are an autonomous trading agent on a small DeFi blockchain with two tokens, USD and WETH.
Venues:
- AMM pool (constant product x*y=k, 0.3% fee). A swap moves the pool price against you; large trades have more price impact.
- Lending market: deposit WETH as collateral and borrow USD up to 75% of its value (valued at the lending oracle price).
  If your debt exceeds 80% of your collateral's value (health factor < 1), anyone can liquidate you: they repay up to half of
  your debt and take collateral worth that plus a 5% bonus.
- The exchange price is the real market price, off-chain. The oracle follows it with a delay; the pool is moved by trades.
Your goal: maximise your net worth in USD (USD + WETH*price + collateral*price - debt) at the end.
Blocks come every few seconds and may be replaced by a competing fork (a reorg); a transaction may be included late.
Reply with ONE JSON object and nothing else:
{"actions": [{"action": "buy_weth" | "sell_weth" | "deposit" | "withdraw" | "borrow" | "repay",
              "amount_usd": <number, for buy_weth/borrow/repay>, "amount_weth": <number, for sell_weth/deposit/withdraw>}],
 "reason": "<one sentence>"}
At most MAX_ACTIONS actions, executed in order. An empty list means hold.""".replace("MAX_ACTIONS", str(MAX_ACTIONS))


class Trader:
    def __init__(self, world: World, llm: LLM):
        self.w, self.llm = world, llm
        self.prices = deque(maxlen=12)
        self.history = deque(maxlen=8)

    def observe(self):
        p, h = self.w.prices(), self.w.holdings()
        ref = p["exchange"] or p["oracle"]
        self.prices.append(round(ref, 1))
        return p, h, ref

    def prompt(self, p, h, ref):
        lines = [f"Block {p['block']}. Time left: {self.w.time_left():.0f} s.",
                 f"Exchange price {ref:.2f} USD/WETH. Oracle {p['oracle']:.2f}. Lending uses {p['lending']:.2f}. "
                 f"Pool {p['pool']:.2f} ({(p['pool'] - ref) / ref * 100:+.2f}% vs exchange), depth {p['reserve_weth']:.1f} WETH.",
                 f"Recent exchange prices, oldest first: {list(self.prices)}",
                 f"Your wallet: {h['usd']:.0f} USD, {h['weth']:.3f} WETH. Lending: collateral {h['collateral']:.3f} WETH, "
                 f"debt {h['debt']:.0f} USD, health factor {'none (no debt)' if math.isinf(h['health']) else round(h['health'], 3)}.",
                 f"Net worth: {h['usd'] + (h['weth'] + h['collateral']) * ref - h['debt']:.0f} USD."]
        if h["debt"] > 0 and h["collateral"] > 0:
            lines.append(f"You are liquidated if the lending price falls below {h['debt'] / (0.8 * h['collateral']):.2f}.")
        if self.history:
            lines.append("Your recent decisions and what happened:\n- " + "\n- ".join(self.history))
        lines.append("What do you do now?")
        return "\n".join(lines)

    def validate(self, plan, h, p):
        """Keep only well-formed actions, clamp amounts to what we hold."""
        out = []
        for a in (plan.get("actions") or [])[:MAX_ACTIONS]:
            if not isinstance(a, dict):
                continue
            kind = a.get("action")
            usd = _num(a.get("amount_usd"))
            weth = _num(a.get("amount_weth"))
            if kind == "buy_weth":
                usd = min(usd, h["usd"] * 0.99)
            elif kind == "sell_weth":
                weth = min(weth, h["weth"] * 0.99)
            elif kind == "deposit":
                weth = min(weth, h["weth"])
            elif kind == "withdraw":
                weth = min(weth, h["collateral"])
            elif kind == "borrow":  # never borrow to the very limit: the price moves while we wait
                room = h["collateral"] * p["lending"] * 0.75 - h["debt"]
                usd = min(usd, room * 0.9)
            elif kind == "repay":
                usd = min(usd, h["usd"], h["debt"] * 1.001)
            else:
                continue
            amount = usd if kind in ("buy_weth", "borrow", "repay") else weth
            if amount > 1e-6:
                out.append({"action": kind, ("amount_usd" if amount is usd else "amount_weth"): round(amount, 6)})
        return out

    def act(self, a):
        kind = a["action"]
        amount = a.get("amount_usd", a.get("amount_weth"))
        {"buy_weth": self.w.buy_weth, "sell_weth": self.w.sell_weth, "deposit": self.w.deposit,
         "withdraw": self.w.withdraw, "borrow": self.w.borrow, "repay": self.w.repay}[kind](amount)

    def step(self, _w):
        p, h, ref = self.observe()
        t0 = time.time()
        try:
            plan = self.llm.chat_json(SYSTEM + "\n" + PERSONA, self.prompt(p, h, ref))
        except Exception as e:
            print(f"[{self.w.name}] LLM error: {e}", flush=True)
            self.w.log_decision([], f"llm error: {e}", error=True)
            return
        latency = time.time() - t0
        actions = self.validate(plan, h, p)
        reason = str(plan.get("reason", ""))[:200]
        results = []
        for a in actions:
            try:
                self.act(a)
                results.append(f"{a['action']} ok")
            except Reverted as e:
                results.append(f"{a['action']} reverted ({e.reason})")
                break  # later steps usually depend on this one
        obs = {"exchange": round(ref, 2), "pool": round(p["pool"], 2), "oracle": round(p["oracle"], 2),
               "usd": round(h["usd"], 2), "weth": round(h["weth"], 4), "collateral": round(h["collateral"], 4),
               "debt": round(h["debt"], 2), "health": None if math.isinf(h["health"]) else round(h["health"], 4)}
        self.w.log_decision(actions, reason, latency=round(latency, 2), results=results, proposed=plan.get("actions"), obs=obs)
        self.history.append(f"block {p['block']}: {json.dumps(actions) if actions else 'hold'} -> {'; '.join(results) or 'held'}")
        print(f"[{self.w.name}] block {p['block']} ({latency:.1f}s) {actions or 'hold'} | {reason}", flush=True)


def _num(x):
    try:
        v = float(x)
        return v if math.isfinite(v) and v > 0 else 0.0
    except (TypeError, ValueError):
        return 0.0


if __name__ == "__main__":
    w = World.from_env()
    w.approve_all()
    llm = LLM()
    print(f"[{w.name}] {w.address}, model {llm.model} at {llm.base_url}", flush=True)
    trader = Trader(w, llm)
    w.run(trader.step, every=EVERY)
    print(f"[{w.name}] done after {llm.calls} LLM calls, net worth {w.net_worth():.0f}", flush=True)
