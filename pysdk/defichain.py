"""defichain: a small Python SDK for agents on the defichain devnet (web3.py underneath).

Run your agent from the simulator, which gives it a funded account and the market:

    go run ./cmd/sim -agent "me=python3 pysdk/examples/momentum.py"

and inside the agent:

    from defichain import World
    w = World.from_env()           # reads DEFICHAIN_* environment variables set by cmd/sim
    w.approve_all()                # once: let the AMM and the lending market pull your tokens
    p = w.prices()                 # {"pool": 1998.7, "oracle": 2000.0, "exchange": 2001.3, ...}
    h = w.holdings()               # {"usd": ..., "weth": ..., "collateral": ..., "debt": ..., "health": ...}
    w.buy_weth(5_000)              # swap 5000 USD for WETH on the AMM (3% slippage limit)
    w.log_decision(actions=[...], reason="...")   # for the summary and your report

Amounts are in tokens (floats); the SDK converts to and from 18-decimal integers.
Everything is a plain function of the chain: read the source, it is short.
"""

import json
import math
import os
import random
import re
import signal
import sys
import time

import requests
from eth_account import Account
from web3 import Web3

E18 = 10**18
MAX_UINT = 2**256 - 1


def to_wei(x: float) -> int:
    """Token amount -> 18-decimal integer, at 1e-6 resolution (exact, no float wei dust)."""
    return int(round(x * 1_000_000)) * 10**12


def from_wei(x: int) -> float:
    return x / E18


class Reverted(Exception):
    """A transaction (or its gas estimate) reverted; .reason is the require() message."""

    def __init__(self, reason):
        super().__init__(f"reverted: {reason}")
        self.reason = reason


class Account_:
    """One account: signs and sends transactions, tracks its nonce, waits for receipts."""

    def __init__(self, w3: Web3, key: str, chain_id: int, tip_gwei: float = 1.0):
        self.w3 = w3
        self.acct = Account.from_key(key)
        self.address = self.acct.address
        self.chain_id = chain_id
        self.tip = int(tip_gwei * 10**9)
        self._nonce = None
        self.confirmations = 0
        self.timeout = 120

    def _rpc(self, method, params):
        resp = self.w3.provider.make_request(method, params)
        if "error" in resp and resp["error"]:
            raise RuntimeError(resp["error"].get("message", str(resp["error"])))
        return resp.get("result")

    def send(self, tx: dict, wait: bool = True, tip_gwei: float = None):
        """Sign and send tx ({"to", "data", "value"}). Estimates gas first: if the transaction
        would revert, raises Reverted without spending anything. Returns the receipt (a dict)
        if wait, else the transaction hash. tip_gwei overrides self.tip for this transaction."""
        tip = self.tip if tip_gwei is None else int(tip_gwei * 10**9)
        tx = dict(tx)
        tx.setdefault("value", 0)
        tx["from"] = self.address
        if "gas" not in tx:
            try:
                tx["gas"] = int(self.w3.eth.estimate_gas({k: tx[k] for k in ("from", "to", "data", "value") if k in tx}))
            except Exception as e:  # web3 raises ContractLogicError with the reason
                raise Reverted(_reason(e)) from None
        for attempt in range(2):
            if self._nonce is None:
                self._nonce = self.w3.eth.get_transaction_count(self.address, "pending")
            tx.update(nonce=self._nonce, chainId=self.chain_id, type=2, maxFeePerGas=tip, maxPriorityFeePerGas=tip)
            signed = self.acct.sign_transaction(tx)
            raw = getattr(signed, "raw_transaction", None) or signed.rawTransaction  # web3 7+ / 6
            try:
                h = self.w3.eth.send_raw_transaction(raw)
                self._nonce += 1
                break
            except Exception as e:
                self._nonce = None  # resync from the node next time
                if attempt == 0 and "nonce too low" in str(e):
                    continue
                raise
        h = "0x" + bytes(h).hex() if not isinstance(h, str) else h
        return self.wait(h, raw) if wait else h

    def wait(self, tx_hash: str, raw=None) -> dict:
        """Poll until the transaction is in the canonical chain with `confirmations` blocks on
        top. A reorg can take a receipt away again; then we keep waiting (and resend)."""
        start = last_seen = time.time()
        while time.time() - start < self.timeout:
            rc = self._rpc("eth_getTransactionReceipt", [tx_hash])
            if rc:
                last_seen = time.time()
                head = int(self._rpc("eth_blockNumber", []), 16)
                if head >= int(rc["blockNumber"], 16) + self.confirmations:
                    if int(rc["status"], 16) != 1:
                        raise Reverted(rc.get("revertReason", ""))
                    return rc
            elif raw is not None and time.time() - last_seen > 15:
                try:
                    self.w3.eth.send_raw_transaction(raw)  # it may have been dropped by a reorg
                except Exception:
                    pass
                last_seen = time.time()
            time.sleep(0.2)
        raise TimeoutError(f"transaction {tx_hash} not mined in {self.timeout}s")


def _reason(e) -> str:
    msg = str(e.args[0]) if getattr(e, "args", None) else str(e)
    for prefix in ("execution reverted: ", "execution reverted"):
        if prefix in msg:
            return msg.split(prefix, 1)[1].strip(" '\"()") if prefix.endswith(": ") else ""
    return msg


class World:
    """The market a simulation deployed (see world.json), seen from one account."""

    def __init__(self, world_path: str, key: str, rpc: list, exchange: str = "", exchange_token: str = "", name: str = "agent"):
        with open(world_path) as f:
            self.info = json.load(f)
        self.name = name
        self.rpcs = rpc or self.info["rpc"]
        self.w3 = Web3(Web3.HTTPProvider(self.rpcs[0], request_kwargs={"timeout": 30}))
        self.chain_id = self.info["chainId"]
        self.account = Account_(self.w3, key, self.chain_id)
        self.address = self.account.address
        c = self.info["contracts"]
        abi = self.info["abis"]
        mk = lambda addr, name: self.w3.eth.contract(address=Web3.to_checksum_address(addr), abi=abi[name])
        self.usd = mk(c["USD"], "Token")
        self.weth = mk(c["WETH"], "Token")
        self.amm = mk(c["AMM"], "AMM")
        self.feed = mk(c["FeedOracle"], "FeedOracle")
        self.lending = mk(c["Lending"], "Lending")
        self.lending_oracle = mk(c["LendingOracle"], "FeedOracle")  # both oracles have price()
        self.params = self.info.get("params", {})
        self.exchange_url = exchange or self.info.get("exchange", "")
        self.exchange_token = exchange_token
        self.decisions_path = os.environ.get("DEFICHAIN_DECISIONS", "")
        self.duration = float(os.environ.get("DEFICHAIN_DURATION", "0") or 0)
        self.started = time.time()

    @classmethod
    def from_env(cls):
        """The world cmd/sim launched this agent into (DEFICHAIN_* environment variables)."""
        need = ["DEFICHAIN_WORLD", "DEFICHAIN_KEY"]
        missing = [k for k in need if not os.environ.get(k)]
        if missing:
            sys.exit(f"missing {missing}: run me from the simulator, e.g.\n"
                     f'  go run ./cmd/sim -agent "me=python3 {sys.argv[0]}"')
        return cls(os.environ["DEFICHAIN_WORLD"], os.environ["DEFICHAIN_KEY"],
                   [u for u in os.environ.get("DEFICHAIN_RPC", "").split(",") if u],
                   os.environ.get("DEFICHAIN_EXCHANGE", ""), os.environ.get("DEFICHAIN_EXCHANGE_TOKEN", ""),
                   os.environ.get("DEFICHAIN_AGENT", "agent"))

    # ---- transactions --------------------------------------------------------------------

    def call(self, fn, wait: bool = True, tip_gwei: float = None):
        """Send a contract call built with web3, e.g. w.call(w.usd.functions.transfer(to, amount)).
        tip_gwei: the priority fee for this transaction (default: w.account.tip, 1 gwei).
        Miners include higher tips first."""
        tx = fn.build_transaction({"from": self.address, "gas": 0, "nonce": 0, "maxFeePerGas": 0, "maxPriorityFeePerGas": 0, "chainId": self.chain_id})
        return self.account.send({"to": tx["to"], "data": tx["data"], "value": tx.get("value", 0)}, wait=wait, tip_gwei=tip_gwei)

    def approve_all(self):
        """Let the AMM and the lending market move your USD and WETH (once, at start)."""
        for tok in (self.usd, self.weth):
            for spender in (self.amm.address, self.lending.address):
                if tok.functions.allowance(self.address, spender).call() < MAX_UINT // 2:
                    self.call(tok.functions.approve(spender, MAX_UINT))

    # ---- reading the market --------------------------------------------------------------

    def block(self) -> int:
        return self.w3.eth.block_number

    def reserves(self):
        """AMM reserves (WETH, USD) as integers."""
        return self.amm.functions.reserve0().call(), self.amm.functions.reserve1().call()

    def prices(self) -> dict:
        """pool: AMM spot price; oracle: the feed; lending: what the lending market uses;
        exchange: the off-chain "real" price (None if there is no exchange)."""
        r_weth, r_usd = self.reserves()
        out = {"pool": r_usd / r_weth if r_weth else math.nan,
               "oracle": from_wei(self.feed.functions.price().call()),
               "lending": from_wei(self.lending_oracle.functions.price().call()),
               "reserve_weth": from_wei(r_weth), "reserve_usd": from_wei(r_usd), "block": self.block()}
        out["exchange"] = self.exchange_price()
        return out

    def holdings(self, who: str = None) -> dict:
        who = who or self.address
        hf = self.lending.functions.healthFactor(who).call()
        return {"usd": from_wei(self.usd.functions.balanceOf(who).call()),
                "weth": from_wei(self.weth.functions.balanceOf(who).call()),
                "collateral": from_wei(self.lending.functions.collateralOf(who).call()),
                "debt": from_wei(self.lending.functions.debtOf(who).call()),
                "health": math.inf if hf == MAX_UINT else from_wei(hf)}

    def net_worth(self, price: float = None) -> float:
        h = self.holdings()
        p = price or self.exchange_price() or self.prices()["oracle"]
        return h["usd"] + (h["weth"] + h["collateral"]) * p - h["debt"] + self.exchange_account_value(p)

    @staticmethod
    def amount_out(amount_in: int, reserve_in: int, reserve_out: int) -> int:
        """Exactly what AMM.getAmountOut computes (0.3% fee), in integers."""
        fee_in = amount_in * 9970
        return fee_in * reserve_out // (reserve_in * 10000 + fee_in)

    # ---- trading -------------------------------------------------------------------------

    def _capped(self, amount: float, balance_wei: int) -> int:
        """to_wei(amount), or exactly balance_wei if it is within 1e-6 of it. A float read
        of a balance (holdings()) converted back is off by up to 0.5e-6: "sell all my WETH"
        would revert half the time (a little above the balance) or leave dust (below).
        Amounts clearly above the balance are kept, so the transaction reverts."""
        x = to_wei(amount)
        return balance_wei if abs(x - balance_wei) <= 10**12 else x

    def buy_weth(self, usd: float, slippage: float = 0.03, wait: bool = True, tip_gwei: float = None):
        """Swap `usd` USD for WETH on the AMM; reverts if the price moved more than slippage."""
        r_weth, r_usd = self.reserves()
        amount = self._capped(usd, self.usd.functions.balanceOf(self.address).call())
        min_out = int(self.amount_out(amount, r_usd, r_weth) * (1 - slippage))
        return self.call(self.amm.functions.swap(self.usd.address, amount, min_out), wait=wait, tip_gwei=tip_gwei)

    def sell_weth(self, weth: float, slippage: float = 0.03, wait: bool = True, tip_gwei: float = None):
        """Swap `weth` WETH for USD on the AMM; reverts if the price moved more than slippage."""
        r_weth, r_usd = self.reserves()
        amount = self._capped(weth, self.weth.functions.balanceOf(self.address).call())
        min_out = int(self.amount_out(amount, r_weth, r_usd) * (1 - slippage))
        return self.call(self.amm.functions.swap(self.weth.address, amount, min_out), wait=wait, tip_gwei=tip_gwei)

    def deposit(self, weth: float):
        amount = self._capped(weth, self.weth.functions.balanceOf(self.address).call())
        return self.call(self.lending.functions.deposit(amount))

    def withdraw(self, weth: float):
        amount = self._capped(weth, self.lending.functions.collateralOf(self.address).call())
        return self.call(self.lending.functions.withdraw(amount))

    def borrow(self, usd: float):
        return self.call(self.lending.functions.borrow(to_wei(usd)))

    def repay(self, usd: float):
        """Repay up to `usd` of your debt (the contract takes at most what you owe)."""
        amount = self._capped(usd, self.usd.functions.balanceOf(self.address).call())
        return self.call(self.lending.functions.repay(amount))

    def liquidate(self, user: str, repay_usd: float):
        amount = self._capped(repay_usd, self.usd.functions.balanceOf(self.address).call())
        return self.call(self.lending.functions.liquidate(Web3.to_checksum_address(user), amount))

    def borrowers(self) -> list:
        n = self.lending.functions.borrowersCount().call()
        return [self.lending.functions.borrowers(i).call() for i in range(n)]

    # ---- what everyone else is doing ------------------------------------------------------

    def recent_trades(self, blocks: int = 10) -> list:
        """The AMM swaps and the liquidations of the last `blocks` blocks, oldest first, from the
        chain's logs (what a block explorer shows): [{"block", "kind": "swap"|"liquidate",
        "who", "weth", "usd", "price"}, ...]. Swaps: weth < 0 sold WETH into the pool, > 0
        bought. Liquidations: who = the liquidator, "user" = the liquidated borrower, weth
        seized, usd repaid. Includes your own trades (compare "who" with w.address)."""
        head = self.block()
        frm = max(1, head - blocks + 1)
        out = []
        for ev in self.amm.events.Swap.get_logs(from_block=frm, to_block=head):
            a = ev.args
            sold_weth = a.tokenIn.lower() == self.weth.address.lower()
            weth = -from_wei(a.amountIn) if sold_weth else from_wei(a.amountOut)
            usd = from_wei(a.amountOut) if sold_weth else -from_wei(a.amountIn)
            out.append({"block": ev.blockNumber, "kind": "swap", "who": a.trader, "weth": weth, "usd": usd,
                        "price": abs(usd / weth) if weth else 0.0})
        for ev in self.lending.events.Liquidate.get_logs(from_block=frm, to_block=head):
            a = ev.args
            out.append({"block": ev.blockNumber, "kind": "liquidate", "who": a.liquidator, "user": a.user,
                        "weth": from_wei(a.seized), "usd": from_wei(a.repaid),
                        "price": from_wei(a.repaid) / from_wei(a.seized) if a.seized else 0.0})
        out.sort(key=lambda e: e["block"])
        return out

    def pending_txs(self) -> list:
        """Transactions waiting in the node's txpool (txpool_content): what the next block may
        contain. Each: {"hash", "from", "to", "nonce", "tip_gwei", "input"}. Anyone can see
        them; that is what front-running (MEV) exploits."""
        res = self.account._rpc("txpool_content", [])
        out = []
        for txs in (res or {}).get("pending", {}).values():
            for tx in txs.values():
                out.append({"hash": tx["hash"], "from": tx["from"], "to": tx.get("to"), "nonce": int(tx["nonce"], 16),
                            "tip_gwei": int(tx.get("maxPriorityFeePerGas") or tx["gasPrice"], 16) / 1e9,
                            "input": tx["input"]})
        return out

    # ---- the off-chain exchange ----------------------------------------------------------

    def exchange_price(self):
        if not self.exchange_url:
            return None
        try:
            return requests.get(self.exchange_url + "/price", timeout=5).json()["price"]
        except Exception:
            return None

    def exchange_trade(self, weth: float) -> dict:
        """Buy (weth > 0) or sell (< 0) WETH on the exchange, in your margin account there."""
        r = requests.post(self.exchange_url + "/trade", json={"weth": weth}, timeout=5,
                          headers={"Authorization": "Bearer " + self.exchange_token})
        r.raise_for_status()
        return r.json()

    def exchange_account_value(self, price: float) -> float:
        if not (self.exchange_url and self.exchange_token):
            return 0.0
        try:
            a = requests.get(self.exchange_url + "/account", timeout=5,
                             headers={"Authorization": "Bearer " + self.exchange_token}).json()
            return a["usd"] + a["weth"] * price
        except Exception:
            return 0.0

    # ---- bookkeeping ---------------------------------------------------------------------

    def log_decision(self, actions: list, reason: str = "", **extra):
        """Append one decision (JSON line) to the decision log the simulator summarises.
        actions: what you decided to do, e.g. [{"action": "buy_weth", "amount_usd": 5000}];
        an empty list is a decision to hold."""
        if not self.decisions_path:
            return
        rec = {"t": round(time.time() - self.started, 2), "block": self.block(), "actions": actions, "reason": reason}
        rec.update(extra)
        with open(self.decisions_path, "a") as f:
            f.write(json.dumps(rec, default=str) + "\n")

    def time_left(self) -> float:
        return self.duration - (time.time() - self.started) if self.duration else math.inf

    def run(self, step, every: float = 5.0):
        """Call step(world) every `every` seconds until the simulation ends (SIGINT/SIGTERM).
        Exceptions in step are printed and the loop goes on: the market does not wait."""
        stop = {"now": False}

        def handler(*_):
            stop["now"] = True

        signal.signal(signal.SIGINT, handler)
        signal.signal(signal.SIGTERM, handler)
        while not stop["now"]:
            t = time.time()
            try:
                step(self)
            except Reverted as e:
                print(f"[{self.name}] {e}", flush=True)
            except Exception as e:  # keep going: one failed round is not the end
                print(f"[{self.name}] step failed: {type(e).__name__}: {e}", flush=True)
            time.sleep(max(0.0, every - (time.time() - t)) * random.uniform(0.9, 1.1))


# ---- LLM helper ------------------------------------------------------------------------


class LLM:
    """Minimal OpenAI-compatible chat client (DeepSeek, Qwen/DashScope, the course router,
    a local Ollama or vLLM, or cmd/mockllm). Configure with LLM_BASE_URL, LLM_MODEL and
    LLM_API_KEY (or LLM_API_KEY_FILE). The key is never printed.

    Reasoning ("thinking") models: thinking is off by default, because a model that thinks
    first can spend its whole token budget on it and return no answer (finish_reason
    "length", empty content), and a trading round should not wait a minute. LLM_THINKING=on
    (or thinking=True) turns it on, with a larger budget; if a reply still runs out of
    tokens while thinking, the call is repeated once without thinking."""

    def __init__(self, base_url=None, model=None, api_key=None, temperature=0.3, max_tokens=None, timeout=None,
                 thinking=None):
        self.base_url = (base_url or os.environ.get("LLM_BASE_URL", "http://127.0.0.1:28199/v1")).rstrip("/")
        self.model = model or os.environ.get("LLM_MODEL", "mock")
        key = api_key or os.environ.get("LLM_API_KEY", "")
        if not key and os.environ.get("LLM_API_KEY_FILE"):
            with open(os.path.expanduser(os.environ["LLM_API_KEY_FILE"])) as f:
                key = f.read().strip()
        self.api_key = key
        self.thinking = os.environ.get("LLM_THINKING", "") == "on" if thinking is None else thinking
        self.temperature = temperature
        self.max_tokens = max_tokens or (16000 if self.thinking else 1000)
        # Seconds to wait for one reply. Thinking can take minutes on a busy server.
        self.timeout = timeout or float(os.environ.get("LLM_TIMEOUT", "") or (600 if self.thinking else 120))
        self.plain = False  # True after the server rejected the optional fields: send only standard ones
        self.calls = 0
        self.last_reasoning = ""  # the reasoning of the last reply, if the server returns it separately
        # Extra request fields, merged into every request (for servers that need their own
        # parameters):  export LLM_EXTRA_BODY='{"top_p": 0.9}'
        self.extra = json.loads(os.environ.get("LLM_EXTRA_BODY", "") or "{}")

    def _body(self, system, user, thinking):
        body = {"model": self.model, "temperature": self.temperature, "max_tokens": self.max_tokens,
                "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}]}
        if not self.plain:
            body["response_format"] = {"type": "json_object"}
            if not thinking:
                # vLLM passes these to the chat template; model families read different keys.
                body["chat_template_kwargs"] = {"thinking": False, "enable_thinking": False}
        body.update(self.extra)
        return body

    def chat(self, system: str, user: str) -> str:
        """One chat completion; returns the reply's content (without reasoning)."""
        headers = {"Authorization": "Bearer " + self.api_key} if self.api_key else {}
        thinking, err = self.thinking, None
        for attempt in range(4):
            if attempt:
                time.sleep(2 ** attempt)
            try:
                r = requests.post(self.base_url + "/chat/completions", json=self._body(system, user, thinking),
                                  headers=headers, timeout=self.timeout)
            except requests.RequestException as e:  # timeout, connection reset: retry
                err = e
                continue
            if r.status_code == 400 and not self.plain:  # some servers reject the optional fields
                self.plain, err = True, RuntimeError(f"LLM HTTP 400: {r.text[:200]}")
                continue
            if r.status_code != 200:
                err = RuntimeError(f"LLM HTTP {r.status_code}: {r.text[:200]}")
                if r.status_code < 500 and r.status_code != 429:
                    raise err
                continue
            self.calls += 1
            choice = r.json()["choices"][0]
            msg = choice.get("message") or {}
            content = msg.get("content") or ""
            self.last_reasoning = msg.get("reasoning_content") or msg.get("reasoning") or ""
            if content.strip():
                return content
            if choice.get("finish_reason") == "length" and thinking:
                err, thinking = RuntimeError("the model used up max_tokens while thinking"), False
                continue
            err = RuntimeError(f"empty reply (finish_reason={choice.get('finish_reason')!r}"
                               + (", reasoning only: the model is still thinking; raise max_tokens or turn thinking off)"
                                  if self.last_reasoning else ")"))
        raise err

    def chat_json(self, system: str, user: str) -> dict:
        """chat(), then parse the first {...} in the reply as JSON (reasoning models'
        <think>...</think> blocks are dropped first)."""
        text = re.sub(r"<think>.*?</think>", "", self.chat(system, user), flags=re.S)
        i, j = text.find("{"), text.rfind("}")
        if i < 0 or j <= i:
            raise ValueError(f"no JSON object in reply: {text[:200]!r}")
        return json.loads(text[i:j + 1])
