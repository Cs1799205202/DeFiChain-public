#!/usr/bin/env python3
"""Check your LLM configuration before an experiment (and wake the model up).

    export LLM_BASE_URL=... LLM_MODEL=... LLM_API_KEY_FILE=~/.llm-key
    python3 pysdk/examples/llm_check.py            # one test call, waits up to 15 minutes
    python3 pysdk/examples/llm_check.py --thinking # the same with thinking on

It prints where it connects (never the key), how long the reply took, whether JSON mode
and the "thinking off" switch were accepted, and the token usage. A model that was scaled
down to zero needs several minutes to start: run this first, and start the simulation
once it answers in seconds.
"""
import argparse
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from defichain import LLM  # noqa: E402

ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
ap.add_argument("--thinking", action="store_true", help="test with thinking on")
ap.add_argument("--wait", type=float, default=900, help="give up after this many seconds (default 900)")
a = ap.parse_args()

llm = LLM(thinking=a.thinking, timeout=a.wait)
print(f"endpoint {llm.base_url}, model {llm.model}, key {'set' if llm.api_key else 'NOT SET'}, "
      f"thinking {'on' if llm.thinking else 'off'}", flush=True)
if not llm.api_key and "127.0.0.1" not in llm.base_url and "localhost" not in llm.base_url:
    sys.exit("set LLM_API_KEY_FILE (a file that contains only the key) or LLM_API_KEY")
system = 'Answer with a JSON object only: {"actions": [{"action": "buy_weth" or "sell_weth", "amount_usd": number}], "reason": string}'
user = "WETH costs 1990 USD on the exchange and 2040 USD in the pool. You have 20000 USD and 10 WETH. Decide."
for i in (1, 2):  # the second call shows the warm latency
    t = time.time()
    try:
        plan = llm.chat_json(system, user)
    except Exception as e:
        sys.exit(f"call {i} failed after {time.time() - t:.0f}s: {e}")
    print(f"call {i}: {time.time() - t:.1f}s, JSON mode and switches {'rejected (plain requests)' if llm.plain else 'accepted'}, "
          f"reasoning {len(llm.last_reasoning)} chars", flush=True)
    print(f"  plan: {plan}", flush=True)
print("ok: the model answers; if call 2 took more than ~10 s, use a longer decision interval (--every)")
