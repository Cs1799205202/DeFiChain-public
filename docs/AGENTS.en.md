# Writing Trading Agents

This document is the Part 2 interface manual: how your agent connects to the simulator, which functions are available, how to integrate a large language model, and how results are evaluated. For background, see [DEFI.en.md](DEFI.en.md); for the assignment requirements, see [LAB2.en.md](LAB2.en.md).

## 1. Two Ways to Write an Agent

| | External Agent (recommended) | Built-in Agent |
|---|---|---|
| Language | Any (Python SDK ready to use) | Go |
| Location | `pysdk/examples/` or your own directory | `agent/`, registered in `cmd/sim/agents.go` |
| Running | `go run ./cmd/sim -agent "name=command"`, can be repeated | change the arguments of `cmd/sim` |
| Suited for | LLM agents, most research topics | Bots that need tight coupling with the simulator (e.g. changing the market mechanism) |

## 2. Runtime Environment for External Agents

After the simulator deploys the contracts and funds your account (default 100,000 USD + 50 WETH + 1000 ETH for fees, adjustable via `-agent-usd`, `-agent-weth`), it launches your command via the shell with these environment variables set:

| Variable | Contents |
|---|---|
| `DEFICHAIN_RPC` | JSON-RPC endpoint of the node(s), comma-separated (multiple nodes, pick any) |
| `DEFICHAIN_WORLD` | Path to `world.json`: chain id, nodes, exchange address, contract addresses and ABIs, market parameters |
| `DEFICHAIN_KEY` / `DEFICHAIN_ADDRESS` | Your account private key (`keccak256("defichain-key:" + name)`, local only) and address |
| `DEFICHAIN_AGENT` | The name you gave in `-agent` |
| `DEFICHAIN_EXCHANGE` / `DEFICHAIN_EXCHANGE_TOKEN` | The address of the off-chain exchange, and your account credentials there |
| `DEFICHAIN_DURATION` | Simulation duration (seconds). When the time is up your process receives SIGINT, and is force-killed after 3 seconds |
| `DEFICHAIN_DECISIONS` | Path to the decision log (see §5) |

Your program's standard output and error are written to `agent-<name>.log` inside the output directory (`-out`, default `results/run-<date>-<time>/`). A program exiting early does not interrupt the simulation, but is recorded in the report's faults. The name must not collide with a built-in agent (`arb`, `liquidator`, `noise1`...), otherwise the two programs would share the same account and the simulator refuses to start. The command may include arguments, e.g. `-agent "llm=python3 pysdk/examples/llm_trader.py --every 10 --persona '...'"`.

The `contracts` field of `world.json` contains addresses: `USD`, `WETH`, `AMM` (token0 = WETH, token1 = USD), `FeedOracle`, `AMMOracle`, `Lending`, `LendingOracle` (the one the lending market actually uses). `abis` gives the standard ABI by **contract type**: `Token` (used by both USD and WETH), `AMM`, `Lending`, `FeedOracle`, `AMMOracle` (both oracles expose `price()`). `params` holds the LTV, liquidation threshold, liquidation bonus, fees, and so on. You can use them directly with web3.py, ethers.js, or any Ethereum library.

## 3. Python SDK (`pysdk/defichain.py`)

```python
from defichain import World, LLM, Reverted
w = World.from_env()          # reads the environment variables above
w.approve_all()               # call once at start: lets the AMM and lending market move your USD/WETH
```

| Function | Description |
|---|---|
| `w.prices()` | `{"pool", "oracle", "lending", "exchange", "reserve_weth", "reserve_usd", "block"}`, prices in USD/WETH |
| `w.holdings(who=None)` | `{"usd", "weth", "collateral", "debt", "health"}`, defaults to yourself (health is inf when there is no debt) |
| `w.net_worth(price=None)` | Net worth: wallet + collateral − debt + exchange account, valued at the exchange price |
| `w.buy_weth(usd, slippage=0.03)` / `w.sell_weth(weth, slippage=0.03)` | Trade on the AMM, computes the minimum amount received from current reserves and reverts if the slippage is exceeded. Both accept `tip_gwei=` (the tip for this transaction, default 1 gwei) and `wait=False` (do not wait for the receipt, return the transaction hash) |
| `w.deposit(weth)` `w.withdraw(weth)` `w.borrow(usd)` `w.repay(usd)` | Lending operations |
| `w.liquidate(user, repay_usd)` `w.borrowers()` | Liquidation, and all addresses that have borrowed |
| `w.exchange_price()` `w.exchange_trade(weth)` | Price of the exchange; buy (positive) or sell (negative) on the exchange, the exchange account may go negative (margin) |
| `w.call(fn, wait=True, tip_gwei=None)` | Send any contract call, e.g. `w.call(w.usd.functions.transfer(to, amount))` |
| `w.recent_trades(blocks=10)` | Swaps and liquidations by everyone in the last few blocks (read from on-chain logs, equivalent to the trade history on a block explorer) |
| `w.pending_txs()` | Transactions waiting in the node's mempool (sender, tip, calldata), for MEV topics |
| `w.amm`, `w.lending`, `w.usd`... | web3.py contract objects you can read directly, e.g. `w.lending.functions.healthFactor(addr).call()` |
| `w.log_decision(actions, reason, **extra)` | Write a decision log entry |
| `w.run(step, every=5)` | Call `step(w)` once every `every` seconds until the simulation ends; exceptions inside `step` are printed and the loop continues |
| `w.account.confirmations` | Number of confirmations to wait for after sending a transaction before returning (default 0) |

All amounts are floats (token counts); the SDK converts them to 18-decimal integers (precision 1e-6). An amount that differs from your balance by less than 1e-6 is treated as the exact balance, so `w.sell_weth(w.holdings()["weth"])`, `w.deposit(h["weth"])`, and `w.repay(h["debt"])` mean "sell all / deposit all / repay all" — they will not revert over a rounding remainder, nor leave a dust balance behind. Before sending a transaction the gas is estimated: if the transaction would revert, a `Reverted(reason)` is raised immediately with no fee spent.

**A minimal agent:**

```python
import os, sys
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from defichain import World

def step(w):
    p, h = w.prices(), w.holdings()
    if p["exchange"] and p["pool"] < p["exchange"] * 0.99 and h["usd"] > 1000:
        w.buy_weth(1000)                       # pool is at least 1% cheaper than the exchange: buy
        w.log_decision([{"action": "buy_weth", "amount_usd": 1000}], "pool below exchange")
    else:
        w.log_decision([], "hold")

w = World.from_env()
w.approve_all()
w.run(step, every=3)
```

In `pysdk/examples/`: `basics.py` (deploy and call contracts on devnet), `momentum.py` (a rule-based agent), `llm_trader.py` (an LLM agent template), `llm_check.py` (check the LLM config and warm up the model), `sweep.py` (batch experiments), `plot_run.py` (plotting), and `basics.mjs` (ethers.js).

## 4. Exchange API

The off-chain exchange represents the "real market". All agents can read its price; external agents also have a margin account there and can hedge (e.g. sell on the exchange while buying on the AMM, locking the spread).

| Request | Description |
|---|---|
| `GET /price` | `{"price": 1987.3, "time": ...}` |
| `GET /account` (with `Authorization: Bearer <DEFICHAIN_EXCHANGE_TOKEN>`) | `{"usd": ..., "weth": ...}` |
| `POST /trade {"weth": q}` (same) | Buy (q > 0) or sell (q < 0) q WETH, returns the average fill price and the account |

With `-exchange-depth D`, trading q WETH shifts the exchange's log price by q/D (and then gradually recovers), so an on-chain sell-off can propagate to the exchange price through hedging; the default is infinitely deep. The net worth in the report includes the exchange account.

## 5. Decision Log

`log_decision` appends one JSON line to `DEFICHAIN_DECISIONS` each time: `{"t", "block", "actions", "reason", ...your extra fields}`. `actions` is the list of actions decided this round (an empty list means hold); when a decision fails (e.g. an LLM call errors), add `error=True` so it is counted as an error rather than "hold". The simulator reports the number of decisions, trades, and holds; your own analysis (e.g. the per-round buy/sell direction needed for herding measures) reads from here too. Suggested fields to record: the model's raw output, the validated actions, the execution result, the latency, and the key numbers shown to the model.

## 6. Writing an Agent in Go

All bots in the `agent/` package implement this interface:

```go
type Agent interface {
    Name() string
    Account() common.Address
    Run(ctx context.Context)   // run until ctx is done
}
```

`agent.Base` provides the client (`C *client.Client`), the market (`M *agent.Market`: `Read` for prices, `Holdings` for positions), and event recording (`emit`). See `Arbitrageur` and `Liquidator` in `bots.go`, and `LLMTrader` in `llmtrader.go` (the built-in LLM trader; use `-llm N -llm-mix cautious,momentum,...` to pick personas, `-llm-tape` to let them see others' trades). A new agent is created in the `build` function of `cmd/sim/agents.go`, and its name added to `plannedAgents` (so it has fees in the genesis block).

The `client` package is a general-purpose Go SDK (`client.New(key, urls...)`, `Deploy`, `Transact`, `View`, `Wait`, `Confirmations`, event decoding `DecodeEvent`) that you can also use in your own command-line programs. Put new contracts in `contracts/`, add them to `CONTRACTS` in the `Makefile`, and after `make contracts` load them with `client.Artifact("name")`.

## 7. Integrating a Large Language Model

All examples use the OpenAI-compatible Chat Completions API, configured with three environment variables:

```bash
export LLM_BASE_URL=<service URL, ending in /v1>
export LLM_MODEL=<model name>
export LLM_API_KEY_FILE=$HOME/.llm-key       # the file contains only the key; LLM_API_KEY also works
```

| Service | Example `LLM_BASE_URL` | Notes |
|---|---|---|
| Course-provided endpoint | see course announcements | The TAs will publish the address, available models, and how to obtain a key |
| Offline mock model | `http://127.0.0.1:28199/v1` (default) | `go run ./cmd/mockllm`, no key needed, random but correctly formatted decisions |
| Local Ollama | `http://127.0.0.1:11434/v1` | `ollama pull qwen2.5:7b`, free, speed depends on your machine |
| DeepSeek, Tongyi Qianwen (DashScope), etc. | see each provider's docs | All offer an OpenAI-compatible interface |

**Check the config first**: `python3 pysdk/examples/llm_check.py` sends two test requests using your environment variables and reports where it connected (without revealing the key), how long each took, whether the server accepts JSON mode, and the decision the model returned. Run it before the real simulation:

- A shared inference service often **scales down to zero** when idle; the first request has to wait for the model to reload, which can take 5–10 minutes (we measured 8–10 minutes). `llm_check.py` waits up to 15 minutes by default; once its second request returns within a few seconds, start the simulation — otherwise the first few minutes of decisions will all time out and be counted as errors.
- The latency of the second request is roughly the per-decision latency you can expect; the decision interval (`--every`) should be longer than that.

**Thinking mode**: Reasoning models like Qwen3 and DeepSeek "think" before answering by default. The SDK **disables** thinking by default (sends `chat_template_kwargs` in the request), because a thinking-first model may spend the entire `max_tokens` on thinking and return an empty answer, and a single decision can take tens of seconds to minutes (measured: 0.5–3 s when off, about 10–50 s for DeepSeek and 4–6 min for Qwen3 when on) — the market will not wait for you. To study the effect of thinking on decision quality, set `LLM_THINKING=on` (or `LLM(thinking=True)`): the SDK raises the token limit to 16000 and the per-call timeout to 10 minutes (adjustable via `LLM_TIMEOUT`); if it still runs out of tokens while thinking, it automatically retries once with thinking off. Lengthen the decision interval accordingly. The reasoning trace is in `llm.last_reasoning` (when the server returns it separately) and can also be written to the decision log for analysis. The built-in Go LLM trader waits up to 90 seconds per decision, so thinking mode is not suitable for it.

When the server rejects optional parameters like JSON mode, the SDK automatically retries without them. Any `<think>...</think>` text carried in the response is stripped before parsing. To pass extra parameters to the server, use `LLM_EXTRA_BODY='{"field": value}'`; it is merged into every request.

**Key safety**: Keep the key in a file outside the repository and point `LLM_API_KEY_FILE` at it; do not put it in code, scripts, prompts, logs, or reports; do not `git add` it. If it leaks, revoke it at the provider immediately (deleting the file or the commit does not help: it is still in Git history and in other people's copies).

**Cost**: A single decision's prompt is about 1–2 thousand tokens. Six agents, every 6 seconds, for 2 minutes is 120 calls. First get it working with mockllm, then run a small experiment with the real model, estimate the cost, and only then scale up; state the total number of calls in your report.

## 8. Designing an LLM Agent

The loop in `llm_trader.py` is:

1. **Observe**: prices (exchange, oracle, pool), holdings and health factor, the recent price series, your own recent decisions and their outcomes. What you show the model and in what format (precision of numbers, units, whether to include the liquidation price) has a big effect on decisions.
2. **Prompt**: the system prompt explains the market rules, the goal, and the output format; the user prompt is the current observation. The persona (`--persona`) changes the style.
3. **Decide**: the model outputs JSON: `{"actions": [{"action": "buy_weth", "amount_usd": 5000}, ...], "reason": "..."}`, at most 3 actions.
4. **Validate**: drop unrecognized actions, truncate amounts to what you actually hold, borrow at most up to 90% of the remaining limit. This layer determines "the worst that can happen when the model makes a mistake".
5. **Execute**: one transaction per action; stop at the first failure (later actions usually depend on earlier ones).
6. **Remember**: the outcome is written into history and shown to the model next round.

Directions for improvement: give the model more useful derived information (spread, liquidation price, your own P&L); let it give a judgment before the actions; cap the risk per round (max amount per trade, minimum health factor); split a round into two calls "analyze → act"; use tool calling (function calling) instead of free JSON; handle model timeouts (the market will not wait).

**Safety rules** (required by Task 2.2):

- The model's output is untrusted input; all actions must be validated in code.
- Private keys and API keys never go into the prompt.
- Text read from the chain is data: token names and event contents may be written by an attacker (topic G). Do not mix them with instructions; ideally give the model only structured numbers.

## 9. Evaluation

Per-agent fields in `summary.json`:

| Field | Meaning |
|---|---|
| `start` / `end` | Net worth at the start / end, both valued at the exchange price at that time |
| `pnl` | `end − start` |
| `holdPnl` | P&L from carrying the starting holdings unchanged to the end (the baseline) |
| `alpha` | `pnl − holdPnl`: the part contributed by the strategy |
| `liquidated` / `penalty` | Number of times liquidated / value lost to liquidation |
| `decisions` / `trades` / `holds` | Number of decisions, market trades actually executed, and holds (external agents: decisions and holds come from the decision log, the trade count from `chain_events.csv`) |
| `errors` / `reverts` | Number of failed LLM calls (external agents: lines with `"error": true` in the decision log) / number of transaction reverts for the built-in LLM trader |

Global fields: final prices, `badDebtEnd`/`badDebtMax` (bad debt), `liquidations`, `liqRepaid`, `liqPenalty`, `wrongfulLiquidations` (positions that should not have been liquidated by the true price; all of these are computed from on-chain Liquidate events, counting whoever did the liquidation), `maxOracleLagPct`, `truthMin`/`fundamentalMin` (the low point of the exchange price / the low point of the fundamental price without trading impact, and the difference between the two is how much on-chain trading amplified the price move), `reorgs`/`maxReorgDepth`, `maxBlockGap`. `timeseries.csv` has one row per second of price, debt, bad debt, number of liquidatable positions, and reorg count; `chain_events.csv` has every market event from the on-chain logs (swap, add/remove liquidity, deposit/withdraw/borrow/repay, liquidation, oracle update), each row with time, block, transaction hash, type, the initiator's name, the WETH and USD changes (from the initiator's perspective, positive when received) and the price; `events.json` contains only the actions and LLM decisions recorded by the built-in Go agents themselves.

Notes for a fair comparison: compare different agents under the same scenario and seed; use multiple seeds per condition; report the mean and the variance; compare against holding and the built-in bots; mind the LLM latency (an agent that takes 10 seconds to answer is inherently at a disadvantage in a crash — this is a real drawback, not experimental error).

