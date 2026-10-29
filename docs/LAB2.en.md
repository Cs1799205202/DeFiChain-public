# Lab 2: PoW Blockchain, DeFi, and AI Trading Agents

Lab 2 of the course "Operating Systems and Distributed Systems".

---

## Objectives

In lecture we covered consensus and consistency. A blockchain puts these problems in the least friendly environment possible: nodes don't trust each other, they join and leave at any time, the network partitions, and anyone can publish a block. Bitcoin and Ethereum (before 2022) reach agreement in this environment with **Proof of Work (PoW)** and the **heaviest-chain rule**, at the cost of offering only **eventual consistency**: a "confirmed" transaction can be undone by a chain reorganization (reorg).

This lab has two steps:

1. **Implement the core of a PoW blockchain yourself**: mining, difficulty adjustment, header validation, block execution and validation, the transaction pool, fork choice, chain reorganization, and block propagation and sync. The framework provides single-transaction EVM execution (the go-ethereum library), P2P networking, and Ethereum JSON-RPC, so the chain you finish can run Solidity smart contracts directly, and web3.py and Foundry can connect to it.
2. **Build a small DeFi economy on your own chain and let AI Agents trade in it**: a decentralized exchange (AMM), lending, an oracle, and liquidations; scripted bots and LLM-driven trading Agents compete in it. You will design your own Agent and run experiments around open questions.

---

## Environment

Everything runs locally; no server, Docker, or accounts are needed.

- **Go 1.26+** (required; an older Go also works, since it downloads the 1.26 toolchain automatically on the first build). If downloads are slow on networks in China, first run `go env -w GOPROXY=https://goproxy.cn,direct`
- **Python 3.10+ and web3.py** (recommended for Part 2): `pip install -r pysdk/requirements.txt`
- Optional: [Foundry](https://book.getfoundry.sh) (`forge`/`cast`, for writing and testing Solidity), solc 0.8.24+, Node.js 20+ (ethers.js)
- The LLM Agent in Part 2 needs an OpenAI-compatible API (the course-provided endpoint, DeepSeek, Qwen, or a local Ollama all work). **You can do most of the development without a key**: `go run ./cmd/mockllm` is an offline fake model

Run all commands from the repository root. Windows users can run the `go ...` commands directly in PowerShell (`make` is only a shortcut, not required).

```bash
go build ./...          # if it compiles, your environment is fine
go run ./cmd/devnet     # start a local 3-node network (Ctrl-C to stop)
```

In the framework as handed out, the ★ files are empty: nodes start and connect to each other, but they don't produce blocks or accept transactions (the logs show `TODO: ... is not implemented yet`). This is expected; Part 1 is about filling them in.

---

## 1. Getting Started

### 1.1 Repository layout

```
chain/          the node (in Part 1 you write the 7 ★ files here)
  pow.go ★  consensus.go ★  process.go ★  txpool.go ★  blockchain.go ★  miner.go ★  sync.go ★
  chain.go       block storage, canonical chain index, and helper functions
  executor.go    EVM: executing a single transaction (applyTx) and eth_call, plus helpers used by process.go
  p2p.go         inter-node communication + fault injection   node.go     assembles the above into a node
  rpc.go         Ethereum JSON-RPC                             admin.go    admin interface (mining, partitions, viewing forks)
spec/           formats for chain parameters, genesis block, and node config (shared by all tools)
cmd/node        node program        cmd/devnet   launch local network + interactive console
cmd/sim         DeFi simulator (Part 2)  cmd/mockllm  offline fake LLM   cmd/labcheck pre-submission self-check
tests/          multi-node tests (real processes)
client/         Go SDK (JSON-RPC client, contract ABIs)    agent/   trading bots and LLM traders written in Go
contracts/      Solidity contracts: Token, AMM, Lending, Oracle (with Foundry tests)
pysdk/          Python SDK and example Agents
docs/           this document, SPEC.md (protocol spec), DEFI.md, AGENTS.md, TOOLS.md
```

In Part 1 you mainly modify the 7 ★ files under `chain/` (you may also create your own `.go` files in `chain/` for helper functions). Code for Parts 2/3 goes in `agent/`, `cmd/sim/`, `contracts/`, `pysdk/`, or new directories you create.

### 1.2 Local network (devnet)

```bash
go run ./cmd/devnet                     # 3 nodes, ports 28001-28003, auto-mining
go run ./cmd/devnet -n 4 -mine=false    # 4 nodes, no auto-mining (mine manually from the console)
```

devnet prints each node's URL, 6 funded dev accounts (alice, bob, carol, dave, eve, deployer, 10000 ETH each), and their private keys. **These private keys are public** (`keccak256("defichain-key:" + name)`) and must only be used on this local network.

Type `help` in the console to see all commands. The most commonly used ones:

```
status                       # each node's head, total difficulty, peers, txpool, reorg count
mine n1 5                    # make n1 mine 5 blocks right now (turn off n1's auto-mining first: miner n1 off)
partition n1 n2,n3           # network partition: {n1} and {n2,n3} can't reach each other
heal                         # restore the network
net all delay=300ms jitter=300ms   # add delay to all nodes' P2P messages (makes forks more likely)
kill n3 / restart n3         # crash / restart a node (nodes don't persist to disk; they resync from scratch after restart)
chain n1 10 / reorgs n1      # show n1's last 10 blocks / the reorgs it has gone through
```

All logs are in files such as `devnet-data/n1.log`, and are also printed to the console in real time.

### 1.3 Interacting with the chain

Nodes implement the standard Ethereum JSON-RPC, so any Ethereum tool works:

```bash
curl -s http://127.0.0.1:28001/status                       # node status (devnet admin interface, JSON)
cast block-number --rpc-url http://127.0.0.1:28001          # Foundry
python3 pysdk/examples/basics.py                            # web3.py: deploy a token, transfer, read events
```

MetaMask: add a network with RPC URL `http://127.0.0.1:28001`, Chain ID `4242`, and currency symbol `ETH`, then import alice's private key. See [TOOLS.en.md](TOOLS.en.md) for how to use more tools.

> Of course, until you finish Part 1 the chain won't produce blocks and transactions won't be included. You can read the code and [SPEC.en.md](SPEC.en.md) first, and come back to try this once Part 1 is done.

### 1.4 Testing

```bash
go test ./chain -run TestPow -v      # one group of unit tests (inside the chain package, a few seconds)
go test ./chain                      # all unit tests
go test ./tests -v                   # multi-node tests: start real node processes (the first run builds the node first; 10+ seconds to a minute)
go run ./cmd/labcheck                # pre-submission self-check: framework files, build, public tests, and score
```

When a test fails it prints the expected and actual values; when a multi-node test fails it prints the last 40 lines of each node's log. `TestFramework*` tests the framework itself and should pass from the start; if those tests fail, you have broken a framework file.

---

## 2. Part 1: Implementing a PoW Blockchain (50 points)

### 2.1 Background: how a PoW chain works

**Accounts and transactions.** As in Ethereum, the state is "address → balance, nonce, contract code and storage". A transaction is signed with a private key and carries an increasing nonce (for an account's n-th transaction, nonce = n), so the same transaction can't be executed twice and each nonce can be used only once. A transaction also declares the maximum gas it may use (`gas`), the maximum it pays per unit of gas (fee cap), and the part of that which goes to the miner as a tip. This chain's base fee is always 0, so the fee = gas actually used × tip, and all of it goes to the miner.

**Execution.** A transaction executes on some state (EVM, the framework's `applyTx`), producing a new state and a **receipt** (success or failure, how much gas was used, which event logs were emitted). Distinguish two kinds of "failure":
- An **invalid** transaction: the nonce isn't the account's next one, the balance can't cover gas × fee cap + transfer value, the gas remaining in the block can't fit it... It can't appear in a block at all;
- An **execution failure** (revert, e.g. a `require` in a contract fails, or gas runs out): it is a valid transaction. It still goes on chain, still pays the fee, and the nonce still increments, but its other changes to the state are rolled back and the receipt's `status` is 0.

**Blocks.** A block header records the parent hash, height, timestamp, difficulty, miner address (coinbase), and commitments to the execution results: the state root (the Merkle root of the entire world state after executing this block), transactions root, receipts root, logs bloom filter, and gas used. Executing a block = executing its transactions in order, then paying the block reward (2 ETH) to the coinbase. Every node that receives a block **re-executes it itself**, and rejects it if the results don't match. So a miner can choose which transactions to include and in what order, but can't forge execution results or give itself money out of thin air.

**Transaction pool.** Users send transactions to any node; the node validates them, puts them in its **transaction pool** (mempool), and forwards them to other nodes. Whoever mines the next block picks transactions from its own pool. Transactions in the pool are queued per account, by nonce: the part with contiguous nonces can execute right away (pending), while those after a nonce gap have to wait (queued). Miners include transactions by tip from high to low, so paying a higher tip gets you on chain faster; sending another transaction with the same nonce and a higher tip **replaces** the one in the pool (this is how speeding up or canceling a transaction works).

**Proof of Work.** Producing a block requires finding a nonce such that `hash(header, nonce) ≤ 2^256 / difficulty`. The expected number of attempts equals the difficulty, and brute force is the only way; verification takes a single hash. When the network's total hash rate is H hashes per second, a block is found on average every difficulty/H seconds, and block intervals are exponentially distributed (so occasionally you wait 10+ seconds; this isn't a bug).

**Difficulty adjustment.** Difficulty adjusts automatically with block speed: it goes up if blocks come too fast and down if they come too slowly, keeping the average block interval near the target (2 seconds in this lab; because timestamps have only one-second precision, it actually settles around 2.5 seconds).

**Forks and the heaviest chain.** When two miners produce blocks at almost the same time, or both sides of a network partition produce blocks independently, a fork occurs. A node stores all valid blocks, forming a tree rooted at the genesis block, and picks the leaf with the **largest total difficulty (the sum of difficulties from genesis to it, i.e. "cumulative work")** as the head. The path from genesis to the head is the **canonical chain**.

```
                    ┌── A3 ── A4              (TD = 1000+...+A4)
genesis ── 1 ── 2 ──┤
                    └── B3 ── B4 ── B5        (branch B is heavier: canonical chain)
```

**Chain reorganization (reorg).** When another branch becomes heavier, the canonical chain switches to it: A3 and A4 are no longer canonical (but remain stored), and B3–B5 become canonical. Transactions on branch A that aren't on branch B go back into the transaction pool to wait for re-inclusion. For users, this means "my transaction was in block A3, and now it isn't". This is why PoW provides only **probabilistic finality**: the more blocks on top of a transaction, the less likely it is to be reorged out, but the probability is never zero.

**Why "heaviest" rather than "longest".** If chains were chosen by length, an attacker could build a very long chain at very low difficulty (difficulty drops automatically based on timestamps). Choosing by total work means forging a heavier chain requires more hash power than the honest miners have, and that is where PoW's security comes from. When two chains are equally heavy, **keep the one seen first** (first seen) until the next block breaks the tie.

**Propagation and sync.** A new block is broadcast to peers, and each node that accepts it forwards it to its own peers (relaying), so nodes don't all need direct connections to each other. Receiving a block whose parent is unknown means you missed the blocks in between, so ask the sender for them. But broadcasts get lost (nodes crash, networks partition), so each node also **periodically asks** its peers for their heads and pulls in any heavier chain it finds. This way, after a partition heals, nodes converge on their own even if nobody produces another block.

### 2.2 What you implement

There are seven tasks, to be done in order, each with corresponding tests. The exact formats and rules (byte layout of hashes, every validation rule, test vectors) are in [SPEC.en.md](SPEC.en.md), and the comments in the framework code are very detailed. **Read `chain/chain.go` and `chain/executor.go` before you start.**

| Task | File | Content | Tests | Approx. size |
|---|---|---|---|---|
| 1 | `pow.go` | Seal hash, PoW hash, target, seal verification, mining (finding a nonce) | `TestPow*` | 60 lines |
| 2 | `consensus.go` | Difficulty adjustment, header validation (10 rules) | `TestConsensus*` | 60 lines |
| 3 | `process.go` | Block execution: miner builds blocks (`build`), validator strictly verifies (`process`), block reward | `TestExec*` | 80 lines |
| 4 | `txpool.go` | Transaction pool: admission checks, queuing by nonce, replacement and ordering by tip, recovery after reorg | `TestTxPool*` | 200 lines |
| 5 | `blockchain.go` | Block insertion, fork choice, chain reorganization | `TestChain*` | 100 lines |
| 6 | `miner.go` | Mining loop, abandoning stale work when the head changes | `TestMiner*` | 80 lines |
| 7 | `sync.go` | Block relay, fetching missing ancestors, periodic sync | `TestSync*`, then `./tests` | 100 lines |

Tasks 1–2 deal only with block headers and tasks 3–4 only with transactions and state; they are independent of each other. Tasks 5–7 put them together, and their tests use all the earlier tasks.

#### Task 1: PoW seal (`pow.go`)

First read the "Seal" section of SPEC.en.md. `SealHash` is the Keccak256 hash of the header without `Nonce` and `MixDigest`; `PowHash(sealHash, nonce)` is `Keccak256(sealHash ‖ 8-byte big-endian nonce)`; a valid seal requires `MixDigest == PowHash` and `PowHash ≤ 2^256/difficulty`. `Seal` is just a loop: start from a random nonce and try one after another, calling `throttle.Wait` every `sealBatch` attempts (this limits the hash rate and also checks whether it's time to stop).

- SPEC.en.md and `TestPowSealHashVector` / `TestPowHashVector` give concrete test vectors: your implementation must match **byte for byte**, otherwise your node and other people's nodes (including the reference implementation used for grading) won't accept each other's blocks.
- Don't modify the header passed in (use `types.CopyHeader`). When the difficulty is nil or ≤ 0, return an error; don't panic.

#### Task 2: Difficulty and header validation (`consensus.go`)

`CalcDifficulty` is Ethereum Homestead's difficulty rule (EIP-2); the formula is in the comments and in SPEC.en.md. `VerifyHeader` checks 10 rules in order, and on each violation returns an error that wraps the corresponding error (the tests check with `errors.Is`), e.g. `fmt.Errorf("%w: got %d, want %d", ErrInvalidGasLimit, a, b)`.

- Watch out for unsigned integers: `time - parent.Time` underflows to a huge number when `time < parent.Time`.
- Rule 3 (the timestamp must not be too far ahead of the local clock) uses the `now` passed in; don't call `time.Now()` yourself. Why is this rule needed? If miners could freely set timestamps in the future, how could difficulty adjustment be manipulated?
- Do the cheap checks first, and verify the seal (`VerifySeal`) last.

#### Task 3: Executing and validating blocks (`process.go`)

Two functions that use the same execution rules:

- `build` (the miner builds a block): execute the candidate transactions in order on the parent block's state, **skipping invalid ones** and executing the rest normally (if one transaction doesn't fit, later smaller ones can still be tried); then pay the block reward to the coinbase and fill in the header fields that depend on the execution results (state root, gas used, transactions root, receipts root, bloom filter). It doesn't write to the state database, because when this block is inserted, `process` will actually execute it once more.
- `process` (the validator): **strictly** re-execute the received block on the parent block's state. The block body must match the header (the transactions root matches, no uncles), and if any transaction is invalid, the whole block is invalid. After execution, compare gas used, bloom filter, receipts root, and state root; only if all of them match, commit the new state (`commitState`) and return the receipts. For any reason for rejection, return an error that wraps `ErrInvalidBlock`.

The specific go-ethereum calls are wrapped in helper functions in `executor.go` (`openState`, `newGasPool`, `applyTx`, `payReward`, `stateRoot`, `commitState`, `txsRoot`, `receiptsRoot`, `logsBloom`, `assemble`); what you write is the block-level logic. `labEVM.block` in `framework_test.go` shows how to use `applyTx` and `commitState` (it only executes transactions; it doesn't pay rewards or check commitments).

- `build` and `process` should share one execution loop (with a strict parameter): the header the miner computes must be exactly identical to the one the validator computes.
- Think about it: when `process` encounters an invalid transaction, why can't it skip it the way `build` does? (Hint: the execution result after skipping is exactly the same as for a block that doesn't contain that transaction at all, but the transactions root says it is in the block.)
- The block reward is paid **after** all transactions: a miner can't spend a block's reward inside the block it is mining.
- `TestExecStateRootVector` pins down the state root of one block: everyone's implementation must produce the same value, otherwise your node and other people's nodes won't accept each other's blocks.

#### Task 4: Transaction pool (`txpool.go`)

You design the internal structure of `TxPool` (the comments have hints). The external methods and rules are fixed, because the framework's RPC, P2P, miner, and `setHead` all depend on them:

- `Add`: first do the state-independent checks (`ValidateTx`: type, chain ID, signature, gas limit, intrinsic gas, fee cap ≥ tip), then check against the head state the nonce (must not be less than the account's nonce) and the balance (must cover gas × fee cap + transfer value). When the same account already has a transaction with the same nonce, the new one replaces it only if its tip is **strictly higher**; otherwise return `ErrReplaceUnderpriced`. When the pool is full (`MaxPoolTxs`), return `ErrPoolFull`. Nonce gaps are fine; queue the transaction first. For a transaction already in the pool, return `(false, nil)`.
- `Pending`: return the executable transactions in the order the miner should try them. Within each account, take transactions contiguously by nonce; across accounts, each time take from the account whose "next" transaction has the highest tip, with ties broken first-come, first-served. This is a miner strategy rather than a consensus rule, but the tests and the Part 2 experiments (e.g. bidding, front-running) depend on this order.
- `PendingNonce`, `Content`: the wallet's `eth_getTransactionCount(addr, "pending")` and `txpool_content`.
- `reset(st, dropped)`: `setHead` calls it every time the head changes. First put the reorged-out transactions back following the rules of `Add` (but checked against the new state), then remove transactions whose nonces the new chain has already used; return the ones that were put back (the node will rebroadcast them).

Notes:
- These methods are called concurrently from different goroutines by RPC, P2P, the miner, and `setHead`, so the data structures must be protected by a lock; but while holding the lock, don't call functions that call back into the pool. Check with `go test -race ./chain`.
- `Pending` can implement the order above with a heap (`container/heap`) whose elements are "each account's next transaction".

#### Task 5: Block insertion, fork choice, chain reorganization (`blockchain.go`)

This is the core of Part 1, and also where most of the hidden tests are. The steps of `insertBlock` are spelled out in the comments: known → parent unknown → verify header → execute (`processBlock`) → store (`writeBlock`, total difficulty = parent's total difficulty + this block's difficulty) → fork choice. Call `reorg` when the new block is heavier than the current head.

`reorg(oldHead, newHead)` does four things: find the **common ancestor** of the two heads; write every block from the ancestor to the new head into the canonical index (`writeCanonical`); delete the heights above the new head that were previously canonical (`deleteCanonical`); and pass the transactions that are on the old branch but not on the new one to `setHead` (which calls the `reset` you wrote in Task 4 to put them back into the transaction pool).

- The most common case is newHead being a child of oldHead, where reorg reduces to "write one canonical block + setHead". But your code must also handle: the new head being on another branch, the new chain being **shorter** (but heavier) than the old one, and a deep fork point (dozens of blocks). Draw these cases on paper before you start.
- On a tie (equal total difficulty), don't switch.
- Invalid blocks must not be stored and must not change the head.
- `insertBlock` already holds `bc.insertMu` while it runs, so you can't call `InsertBlock`/`InsertChain` inside it (it would deadlock; the tests detect this and tell you). The read functions in `chain.go` (`GetBlock`, `GetTd`, etc.) can be called freely.

#### Task 6: Miner (`miner.go`)

`mine` builds a block on a given parent (timestamp, difficulty, coinbase; transactions come from `pool.Pending()`, and `BuildBlock` executes them and fills in the remaining fields), seals it, inserts it, and broadcasts it. `loop` calls `mine` repeatedly and **cancels** the current mining **immediately when the head changes** (subscribe with `bc.SubscribeHead()`); it also restarts every `recommit` seconds so that new transactions get included.

- The timestamp comes from the local clock (`m.bc.Now()`; tests replace the clock), but is at least the parent's time + 1. If the timestamp computed this way exceeds MaxFutureTime, wait, rather than building a block that you yourself wouldn't accept.
- Why must you abandon the work immediately when the head changes? What happens if you don't?

#### Task 7: Propagation and sync (`sync.go`)

`HandleBlock` handles blocks pushed by peers: ignore known blocks (this is the condition that terminates relaying; otherwise blocks would be forwarded between nodes forever); for a block whose parent is unknown, ask the sender for its ancestors (`fetchChain`); forward valid new blocks to all peers except the sender; drop invalid ones. `fetchChain` uses `RequestAncestors` to request backward from some block, at most `MaxAncestors` at a time, until it connects to a block already known locally, then inserts them from oldest to newest. `SyncOnce` asks all peers for their heads and pulls the chain from the one with the largest total difficulty; if that fails, it tries the next one. The framework calls `SyncOnce` once per second, and also once when the node starts.

- Don't trust peers: the total difficulty a peer reports is only a hint, and the blocks it sends may be non-contiguous or wrong. Check them before inserting; `InsertBlock` validates everything on insertion.
- The unit tests use a fake network (`labFakeNet`) with no timing issues. Once they pass, run `go test ./tests -v`, which is a real multi-process network.

### 2.3 Debugging tips

- **Run the unit tests first, then the multi-node tests**, and finally try things by hand with devnet. When a multi-node test fails, look at the node logs it prints first. The framework only logs `REORG ...` (on every reorg) and the WARNING below; **your own code should log at key points** (using `bc.log`, `m.log`, `h.log`): mining a block (height, hash, state), rejecting a block (reason), how many ancestors were requested from whom. With good logs, multi-node problems are often obvious at a glance.
- Reproduce problems in devnet: `go run ./cmd/devnet -mine=false`, then step through with `partition`, `mine`, `heal`, `status`, `chain`, `reorgs`, watching each node's state. `curl http://127.0.0.1:28001/admin/blocks` shows the whole block tree a node has stored.
- When node states diverge, compare the output of `/admin/chain`: at what height do they start to differ? What blocks does each side have at that height?
- Seeing `WARNING: setHead(...): that block is not in the canonical index` means you called `setHead` before updating the canonical index; the order is reversed.
- `processBlock rejects the block that BuildBlock made` means your `build` and `process` follow different execution rules (rewards, gas, which transactions were skipped): the two should share the same execution loop.
- A transaction was sent but never makes it on chain: use `txpool_content` (see Section 6) to see whether it is pending or queued; `chain n1 5` shows how many transactions the most recent blocks contain.
- Use `go test -race ./chain` to check for data races.

### 2.4 Grading

| Part | Points | Notes |
|---|---|---|
| Public tests | 15 | `chain/*_test.go` and `tests/*_test.go`, which you already have; `go run ./cmd/labcheck` computes this part of the score |
| Hidden tests | 35 | Same interfaces and spec as the public tests, with more scenarios; see the table below |

The hidden tests only use behavior specified in SPEC.en.md and the code comments, and don't examine implementation details (you can organize the code inside the ★ files however you like). The scenarios they cover:

| Area | Hidden test scenarios |
|---|---|
| PoW | Seal hashes and PoW hashes of many random headers; edge cases such as difficulty 1, 0, and nil; tampered seals |
| Difficulty and validation | Thousands of parameter sets compared against the reference implementation (including non-increasing timestamps and huge time gaps); each of the 10 rules violated individually, plus valid cases exactly on the boundary |
| Execution | With contract deployments, events, reverts, and various transaction types mixed together, the blocks you build are **bit-for-bit** identical to the reference implementation's, and so are the receipts; timing and amount of the block reward; skipping and continuing when block gas is full; various invalid transactions hidden in blocks; rejected blocks leave no state behind |
| Transaction pool | Every admission rule and its boundaries (exactly affordable, short by exactly 1 wei); replacement of queued and executable transactions; ordering with multiple accounts, different tips and fee caps, and a mix of new and legacy transaction types; recovery after two blocks are reorged out and the new chain uses some of the nonces and moves away one account's balance; full pool; concurrent calls |
| Fork choice and reorg | A shorter but heavier chain wins; ties keep the first seen; after a reorg, the index for every height and every transaction is correct; 30-block-deep reorgs; two branches repeatedly taking turns in the lead; blocks with future timestamps; invalid blocks and their children; concurrent insertion |
| Sync | Fetching hundreds of blocks at once (multiple requests); relay rules (valid side-chain blocks are forwarded, invalid and known ones are not); choosing peers by total difficulty rather than height; peers crashing or sending non-contiguous blocks |
| Miner | Never produces blocks with future timestamps; start/stop; transactions that arrive during mining get included |
| Multi-node | Convergence after a partition heals **with no new blocks**; multi-hop relay in a line topology; a new node joining syncs 300 blocks; a double spend on the two sides of a partition, where only one takes effect after healing; after a reorg, blocks and receipts seen through JSON-RPC change accordingly and transactions are re-included; convergence after natural forks on a high-latency network; deploying and calling contracts through different nodes, with every node returning consistent receipts, event logs, and contract state |

Note 1: The above is the reference implementation and the standard test cases. You may also choose not to follow the framework and instead write your own blockchain from scratch that better fits your ideas, but it must have at least the same level of completeness and amount of work. A self-written chain may not pass the tests, so you need to demonstrate that your implementation is also correct in these test scenarios (for example, by writing your own test cases).

---

## 3. Part 2: On-chain DeFi and AI Trading Agents (40 points)

Part 1 lets you understand a chain from the inside. Part 2 puts you on the user side of the chain, so you can see clearly how an on-chain economic system works, how it goes wrong, and what an AI Agent can do here and what mistakes it makes.

Note 2: As with Note 1, what follows is one possible route through Part 2. You may also choose to do your own research and design. Part 2 is open-ended and interesting ideas are strongly encouraged, but it should involve a comparable amount of work. If you choose your own design, you must additionally submit a written proposal of no more than one page describing your ideas and work, and you should explain it fully to the TA at the final check.

### 3.1 Background: why study "on-chain + Agents"

**DeFi (decentralized finance)** implements financial functions such as exchanges, lending, and liquidation as smart contracts deployed on chain. The rules are fully public and enforced by code, and anyone can participate: there is no account opening and there are no business hours. But it also brings problems that are rare in traditional finance:

- **Where do prices come from?** An on-chain lending protocol needs to know what the collateral is worth, and this price is provided by an **oracle**. An oracle whose quotes lag, or one that simply reads the AMM's spot price, can be manipulated or can trigger wrongful liquidations.
- **Liquidations cascade.** Price drops → undercollateralized positions are liquidated → liquidators sell the seized collateral into the market → price drops further → more positions are liquidated.
- **Transaction ordering can be bought and sold.** Miners order transactions by tip, and transactions are visible to everyone before they are included (the transaction pool is public). This gives rise to front-running and sandwich attacks, known as **MEV**.
- **"Confirmed" doesn't mean "final".** The chain reorgs you implemented yourself in Part 1 mean that a transaction that already has a receipt can be undone.

**AI Agents** are starting to read markets, make decisions, and sign and send transactions on their own. This raises new questions: will large models **trade in the same direction** because of similar training data and prompts, amplifying volatility? Can they understand liquidation risk and confirmations? Anyone can write on-chain data (token names, event contents); if it is put straight into a prompt, can it cause **prompt injection**? When a model "hallucinates" a wrong transaction, who catches it?

Experimenting with these questions on a real chain is both expensive and dangerous. This lab gives you a fully controllable sandbox: your own chain, a complete set of DeFi contracts, and a crowd of bots with different behaviors. You can freely tune the network, the market, and the participants, and run experiments repeatedly. For DeFi basics (AMM formulas, lending and liquidation, oracles, MEV), see [DEFI.en.md](DEFI.en.md), **which we recommend reading first**; the full interface for writing Agents is in [AGENTS.en.md](AGENTS.en.md).

### 3.2 The simulator: a small DeFi economy

`cmd/sim` starts a PoW devnet (using the node you wrote in Part 1), deploys the contracts, and then lets a crowd of Agents trade for a while:

| Component | Description |
|---|---|
| Tokens | USD and WETH (two ERC-20s) |
| AMM | Constant-product market maker (the core of Uniswap V2: `x·y = k`, 0.3% fee), initially 500 WETH / 1 million USD, price 2000 |
| Lending market | Deposit WETH, borrow USD: you can borrow at most 75% of the collateral value; when debt exceeds 80% the position can be liquidated, and the liquidator repays up to half of the debt and takes collateral of equal value plus a 5% bonus |
| Oracle | Feed: a reporter writes the exchange price on chain every second (like Chainlink); Spot: reads the AMM's spot price directly (manipulable; `-spot-oracle` makes the lending market use it) |
| Exchange | The off-chain "real market" (think of Binance); the price follows a random walk and crashes at a configured time (by default starting at 25 seconds, falling 30% over 20 seconds) |
| Built-in Agents | Arbitrageurs (pull the AMM price back to the exchange price and hedge on the exchange), liquidators, noise traders, a few leveraged borrowers who never rebalance, the reporter; optional: built-in LLM traders (`-llm N`) |
| Your Agent | A program in any language; the simulator launches it as a subprocess and gives it a funded account and all contract addresses (`-agent "name=command"`) |

```bash
go run ./cmd/sim                                     # default scenario, 60 seconds, prints a report at the end
go run ./cmd/sim -out results/run1                   # also write the data to results/run1/
go run ./cmd/sim -agent "me=python3 pysdk/examples/momentum.py"            # add your Agent
go run ./cmd/mockllm &                                                      # offline fake model
go run ./cmd/sim -agent "llm=python3 pysdk/examples/llm_trader.py"         # LLM Agent
go run ./cmd/sim -miners 4 -delay 800ms -jitter 800ms -confirmations 3     # slow network: frequent forks and reorgs
```

Common parameters (`go run ./cmd/sim -help` lists them all): `-duration`, `-seed`; price path `-crash-at -crash-for -crash-pct -rebound -volatility`; market `-pool-weth -spot-oracle -exchange-depth`; population `-borrowers -ltv-min -ltv-max -noise -liquidators -no-arb -llm -llm-mix`; network `-miners -hashrate -delay -jitter -drop -partition-at -partition-for -confirmations`.

In the report printed at the end of a run, each Agent has `pnl` (profit and loss, valued at the final exchange price), `hold-pnl` (the PnL of doing nothing and holding the initial assets), and `alpha = pnl − hold-pnl`. In a market that falls 30%, everyone loses money; **only alpha tells you how good the strategy itself is**. The raw material for analysis is in the output directory: `timeseries.csv` (per-second prices, debt, bad debt, reorg count), `chain_events.csv` (**every** swap, deposit, withdrawal, borrow, repayment, liquidation, and oracle update read from the on-chain logs, including your Agent's), `events.json` (actions and decisions recorded by the built-in Agents themselves), `summary.json`, and your Agent's logs (`agent-<name>.log`, `.decisions.jsonl`). `python3 pysdk/examples/plot_run.py results/run1` draws an overview chart.

> A run starts 3 node processes and a dozen or so Agents, and uses several CPU cores on a laptop. The simulation runs in real time: a 60-second scenario takes 60 seconds, plus about half a minute for deployment and wrap-up. Without `-out`, results are written to `results/run-<date>-<time>/`.

### 3.3 Task 2.1: Warm-up (6 points)

Answer briefly in your report (a few sentences, a chart, or some command output per question is enough):

1. **Your chain runs contracts.** Use web3.py, ethers.js, or Foundry to deploy an ERC-20 token on your devnet (you can simply use `contracts/Token.sol`, or write your own), mint, transfer, and read the `Transfer` events. `pysdk/examples/basics.py` is a ready-made starting point.
2. **Understand a simulation.** Run the default scenario once and use `plot_run.py` to plot the three price curves: exchange, oracle, and AMM (by default the lending market uses the oracle price; with `-spot-oracle` it uses the AMM price, and the chart gets an extra line). Explain: why does the AMM price follow the exchange price? Why does the oracle lag? When do the liquidations happen, and why those particular borrowers?
3. **The cost of an AMM.** How much USD do you get for selling 20 WETH into a pool of 500 WETH / 1,000,000 USD? How much less is that than at a price of 2000, and how much of the difference is fees and how much is price impact? What about selling 100 WETH? Compute it with the formulas in DEFI.en.md first, then verify on chain: in `go run ./cmd/sim -noise 0 -no-arb -borrowers 0 -crash-at 0 -volatility 0 -agent-weth 150 -agent "me=python3 your_script.py"` nobody else touches the pool, so your script can compare the formula against the actual fill using `w.reserves()` and `w.sell_weth(20)`.
4. **See a reorg with your own eyes.** Start with `go run ./cmd/devnet -easy -mine=false` (`-easy`: very low, nearly constant difficulty, so manual mining completes instantly and every block has about the same weight), then do the following in order in the console:
   1. `partition n1 n2,n3`;
   2. In another terminal, send a transfer to n1, e.g. `cast send <bob's address> --value 1ether --private-key <alice's private key> --rpc-url http://127.0.0.1:28001 --async` (`--async` makes it return the transaction hash immediately without waiting for the receipt);
   3. `mine n1 1`, then use `cast receipt <tx hash> --rpc-url http://127.0.0.1:28001` to confirm that the transaction is in block 1 on n1;
   4. `mine n2 2`, then `status`: compare the total difficulty of the two sides' heads and predict who will win after healing;
   5. `heal`, wait a second or two, and check the receipt on n1 again;
   6. `mine n2 1`, and check once more.

   What happened after each step? Explain the whole process using `reorgs n1`, `chain n1`, and the node logs. If a trader treated the receipt seen in step 3 as a "fill", what would the consequences be? (Why `-easy`: the default difficulty drops over time, so one block mined within a few seconds can be heavier than two blocks mined tens of seconds later. Think about what this tells you.)

### 3.4 Task 2.2: Your first trading Agent (12 points)

Write two Agents and compare them in the same market:

- **A rule-based Agent**: you can start from `pysdk/examples/momentum.py` (trend following), or design your own (arbitrage between the AMM and the exchange, mean reversion, a leveraged long with automatic deleveraging...). This is your baseline.
- **An LLM Agent**: start from `pysdk/examples/llm_trader.py`. Its structure is "observe → build prompt → model returns a JSON plan → **code validation** → execute → remember the result". At a minimum, you must improve its observations (what the model gets to see), its prompt, and its validation layer, and explain your reasons.

Evaluate under the default crash scenario and a V-shaped scenario (`-rebound 0.2`), with **at least 3 random seeds** (`-seed`) per scenario. `pysdk/examples/sweep.py` shows how to run batches and aggregate the results. The report should include:

- The design of both Agents: what they observe, which actions they can take, the structure of the prompt, and the validation and risk-control rules.
- Results: pnl, alpha (relative to holding), number of times liquidated, number of trades, number of LLM calls and average latency. Give the mean and the range of variation across seeds, and compare them alongside the built-in bots.
- At least 3 concrete LLM decision cases (`agent-<name>.decisions.jsonl` records the input summary, plan, and execution result of every decision): one that went well, one that went badly, and one that your validation layer blocked or modified. Why did the model decide the way it did?

A few principles you must follow:

- **The model proposes, the code decides.** The model's output is untrusted input: check both the action type and the amount, and truncate or reject anything that exceeds the balance or the risk limits.
- **Private keys and API keys never go into prompts**, and must not be written into code or committed to Git (use environment variables or `LLM_API_KEY_FILE`; see AGENTS.en.md).
- Any text read from the chain (token names, events) is **data**, not instructions.
- This task is not judged by how much money you make (beating holding during a crash is hard), but by whether the Agent is reliable and whether your evaluation is fair and honest.

### 3.5 Task 2.3: Research project (22 points)

Pick **one** of the topics below (or propose your own), pose a question that can be answered with data, design experiments, and answer it. Each topic gives a starting point; what exactly to ask and how to measure it is up to you.

**A. Herding and information cascades.** When several LLM Agents can all see the public trade record (the built-in LLM traders use `-llm-tape`; your own Agent can read on-chain trades with the SDK's `w.recent_trades()`), do they trade in the same direction and amplify the crash? What is the difference between identical personas and diverse personas? You can quantify this with the LSV herding measure (Lakonishok, Shleifer & Vishny, 1992) or the "fraction of same-direction decisions", and compare the drop in the exchange price against a control group without LLMs.

**B. Oracle design and liquidation cascades.** When the lending market uses the AMM spot price as its oracle (`-spot-oracle`), how do pool depth (`-pool-weth`) and exchange depth (`-exchange-depth`, which lets on-chain selling pass through to the exchange price) affect the number of liquidations and the bad debt? (The liquidation statistics in `summary.json` and `chain_events.csv` both come from the on-chain logs, so liquidations done by your attacker Agent are included.) Going further: write an attacker Agent that triggers liquidations by manipulating the spot price and profits from them; then design a defense (e.g. a TWAP time-weighted price, written as a new oracle contract) and show experimentally that it works.

**C. Reorg risk and confirmations.** On a high-latency network (`-delay`, `-jitter`, `-partition-at`), forks and reorgs are frequent. How often is an Agent that treats a receipt as a "fill" wrong? How does that fraction change with the number of confirmations it waits for (`-confirmations`, or logic in your own Agent)? Write an Agent that tracks its "true position" using confirmation counts and log events with `removed: true`, and quantify the trade-off between latency and safety.

**D. 51% attacks and double spending.** Give one miner far more hash power than everyone else (`-hashrate`), and have it secretly mine a fork that doesn't contain its own payment transaction. Once a "merchant" Agent ships the goods after k confirmations, publish the fork. How does the attack success rate vary with the share of hash power and with k? Compare with the probability formula in Section 11 of the Bitcoin whitepaper (Nakamoto, 2008). (This requires modifying the miner; see the notes in Part 3.)

**E. Selfish mining.** Implement the selfish mining strategy of Eyal & Sirer (2014): when you mine a block, don't publish it right away; release it at an opportune moment. Measure the selfish miner's share of revenue as a function of its share of hash power, find the threshold at which it becomes profitable, and compare with the theoretical curve in the paper.

**F. MEV: front-running and sandwich attacks.** Miners include transactions by tip from high to low, and transactions waiting to be included are visible to everyone (`txpool_content`, the SDK's `w.pending_txs()`). Write an Agent that watches the transaction pool for large swaps, buys in front of them with a higher tip, and sells behind them with a lower tip (the SDK's trading functions all have `tip_gwei` and `wait=False` parameters). How much does the victim lose? Which defenses work (tighter slippage tolerance, splitting orders, commit-reveal)?

**G. On-chain prompt injection.** Anyone can write text on chain: deploy a token with "instructions" hidden in its name, or write text into transaction data or events. If an Agent puts this content into its prompt, can it be induced to make harmful trades? How susceptible are different models and different prompt structures? Design and evaluate defenses (separating data from instructions, structured input, output validation, least privilege). **Do this only on your own local network.**

**H. Models and prompting strategies.** Compare models of different sizes or types (as well as reasoning modes, chain-of-thought, reflection, longer history, tool calling) on performance, decision latency, and cost in the same market. Control variables, repeat experiments, and report the API cost of each run.

**I. The economics of market making.** Write an LP Agent that adds and removes liquidity in the AMM (the contract already supports `addLiquidity`/`removeLiquidity`), and measure impermanent loss against fee income. Under what volatility and what price paths is providing liquidity worth it?

**J. Your own choice.** New DeFi protocols (stablecoins, prediction markets, auctions), cooperation or competition among multiple Agents, experiments on the PoW mechanism itself... any interesting idea.

**Methodology requirements** (whichever topic you choose):

- Write down the question and your **hypothesis** before running experiments; design **control groups** (no LLM, a rule-based Agent, holding, before/after a fix).
- **At least 3 seeds** per condition; report the mean and the dispersion. With many conditions, consider confidence intervals or significance tests. A single run proves nothing: block timing, transaction ordering, and LLM output are different every time.
- Change one variable at a time; record the full parameters of every run (they are in `summary.json`) so that others can reproduce it.
- Report results that don't match your expectations honestly, and try to explain them. **A rigorously designed experiment that concludes "no effect" scores the same as one that finds a significant effect.**
- Keep costs under control: get the whole pipeline working with `mockllm` first, then switch to a real model; report the total number of calls and the total cost.

### 3.6 Part 2 grading

| Item | Points | Criteria |
|---|---|---|
| Warm-up 2.1 | 6 | All four questions answered correctly, with evidence (charts, numbers, logs) |
| Agent 2.2: implementation | 6 | Both Agents run reliably; the LLM Agent's validation and risk controls are sound |
| Agent 2.2: evaluation | 6 | Multiple seeds, baselines, complete metrics; concrete case analysis with insight |
| Research 2.3: question and design | 8 | Clear, testable question; hypotheses and controls, with controlled variables |
| Research 2.3: results | 8 | Credible data, clear charts, statistically meaningful comparisons |
| Research 2.3: discussion and reproducibility | 6 | Explains the mechanisms, discusses limitations and threats to validity; provides reproduction commands and configurations |

Agent PnL is not graded; what is graded is the quality of the design, the experiments, and the analysis.

---

## 4. Part 3: Innovation and Extensions (bonus, up to 10 points)

Optional. Building on Parts 1 and 2, do something you find interesting that involves a reasonable amount of work. Below are some directions and reference caps on points:

| Direction |
|---|
| A different consensus: implement BFT (e.g. Tendermint) or PoS, and run experiments comparing it with PoW on reorgs, block latency, and impact on DeFi |
| Selfish mining / uncle blocks and GHOST: implement and analyze the revenue (can be combined with Part 2 topic E and taken further) |
| New DeFi protocols (stablecoins, prediction markets, TWAP oracles, derivatives), with Agent experiments that test their design |
| A block explorer, or a visualization of forks and reorgs (e.g. integrated with Lab 1's DistVis platform) |
| Node persistence (no chain loss on restart), snapshot sync, light nodes and Merkle proofs |
| Memory-hard PoW (a simplified Ethash), comparing CPU and memory characteristics |
| Substantial improvements to the Agent framework: tool calling, long-term memory, communication and cooperation among multiple Agents |
| Transaction pool and block-building policy: fair ordering (first-come first-served, batch auctions), front-running-resistant (MEV) block building, private transaction pools, measuring the impact on Agent PnL with the Part 2 simulation |
| Replacing framework modules: implement your own P2P transport (TCP, gossip topology) or JSON-RPC service, or swap in a different execution engine; the public multi-node tests must still pass after the replacement |

Points are awarded based on difficulty, completeness, and depth of analysis. Describe what you did and how you verified it in a separate section of your report.

**Don't break Part 1 grading.** Part 1 is graded using the original framework plus your 7 ★ files. If you need to change the default policies of the consensus, miner, or transaction pool, or extend the framework (including topics D and E), do it on a separate Git branch (e.g. `git checkout -b part3`), keep the ★ files on the main branch in a state that passes the tests, and state the branch name in your report.

---

## 5. Submission

Submit the complete repository on the course platform. The main branch should contain:

- The 7 ★ files you implemented under `chain/` (plus any helper files you created);
- The code for Parts 2 and 3 (your Agents under `pysdk/`, changes to `cmd/sim` or `agent/`, new contracts, etc.), along with instructions for running them;
- **The lab report** `report/REPORT.md` (or a PDF, with the main text no longer than 12 pages): the three Part 2 tasks, Part 3 (if any), and a statement of how you used AI tools;
- Reproduction instructions: the command for every figure and every table (or a `report/reproduce.sh`). Submit only aggregated data (`summary.json`, `runs.csv`, charts), not large raw logs;
- (Optional) a demo video.

Before submitting, run `go run ./cmd/labcheck`: it checks whether the framework files have been modified and whether the code compiles, then runs the public tests and gives the score for that part.

**Never submit**: API keys (use `LLM_API_KEY_FILE` pointing to a file outside the repository; `.gitignore` already ignores `.llm-key`), private keys for real funds, raw data under `devnet-data/` and `results/`, or compiled binaries. If you accidentally commit a key, deleting the file is not enough (it is still in the Git history): revoke the key with the provider immediately.

### 5.1 Overall grading

| Part | Points |
|---|---|
| Part 1 public tests | 15 |
| Part 1 hidden tests | 35 |
| Part 2 warm-up / first Agent / research project | 6 / 12 / 22 |
| Part 3 innovation and extensions (bonus) | up to 10 |
| **Total** | **100** |

Note that an in-person final check will be held before the end of week 18 of the semester (after the final exam). The TAs will schedule the times, and each group must demo its work to a TA (15 min per group). The final lab grade is a weighted combination of the scores above (mainly for reference) and the demo score (more important).

---

## 6. FAQ

**Q: My nodes don't produce blocks?**
Check the logs. If `TODO: ... is not implemented yet` still appears, Part 1 isn't finished. If nothing appears after `miner started`, auto-mining may be turned off (check the mining column in `status`), or your `mine` keeps returning an error (print the errors in `loop`).

**Q: Sometimes no block appears for 10+ seconds. Is my code slow?**
Not necessarily. PoW block intervals are exponentially distributed: with an average of 2.5 seconds, an interval longer than 10 seconds happens about once every 50 blocks. Looking at the average interval over a stretch of time with `chain n1 50` is more meaningful than staring at a single interval.

**Q: Two nodes are stuck at the same height with different hashes, and nothing changes?**
If their total difficulties are equal, this is **correct**: on a tie, each keeps the block it saw first until someone mines the next block and breaks the tie. After `mine n1 1` they should agree. If their total difficulties differ and they still don't converge, there is a problem with sync (Task 7).

**Q: `TestExecStateRootVector` fails?**
It first checks the balances of four accounts (miner = block reward + all fees), then checks the state root. If a balance is wrong, look at whose balance the message reports and how far off it is (off by 2 ETH means the reward; off by tens of thousands of gwei means the fees). If all balances are correct but the state root is wrong, something else in the state differs (nonces, extra accounts): make sure you change the state only through `applyTx` and `payReward`, and that each transaction is executed exactly once.

**Q: A test reports "did not return within ...: a deadlock?"**
Usually this means you called `InsertBlock`/`InsertChain` inside `insertBlock` (`insertMu` is already held), or the transaction pool's lock wasn't released, or you tried to acquire the same lock while already holding it (Go's `sync.Mutex` is not reentrant), or something is waiting on a channel that has no receiver. Run with `go test -timeout 60s`; on timeout, Go prints the stack of every goroutine, so you can see where they are stuck.

**Q: I sent a transaction, but there is never a receipt?**
Check in order: is the chain producing blocks? Does the sender have enough ETH to pay the fee? Is there a gap in the nonces (if an earlier transaction didn't get in, later ones stay queued forever)? `curl -s -X POST localhost:28001 -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"txpool_content","params":[]}'` shows what is waiting in the transaction pool.

**Q: MetaMask complains that the nonce is wrong?**
Every devnet restart is a new chain, and MetaMask still remembers the nonce from the old chain. Reset the account in MetaMask under "Settings → Advanced → Clear activity and nonce data".

**Q: The LLM is too expensive / too slow / I don't have a key?**
During debugging, always use `go run ./cmd/mockllm`. It is offline, free, and instantly returns well-formed random decisions (and occasionally returns invalid JSON on purpose, to test your error handling). For the real experiments, use a cheap model and set a longer decision interval (`llm_trader.py --every 15`), and do one run first to estimate the cost. You can also use Ollama locally (AGENTS.en.md explains how to configure it).

**Q: Every decision of my LLM Agent errors out or times out?**
First run `python3 pysdk/examples/llm_check.py`. The shared model service scales down when nobody is using it, so the first request may take 5–10 minutes; wait until it responds within a few seconds before starting a simulation. An empty answer usually means a reasoning model spent all its tokens on "thinking": the SDK disables thinking by default, and if you turn it on (`LLM_THINKING=on`), set the decision interval to more than a minute (AGENTS.en.md §7).

**Q: Same `-seed`, but two runs give different results?**
The seed fixes only the exchange price path and the randomness of the scripted bots. Block timing, transaction ordering, and LLM answers are different every time, so you must repeat runs and look at the statistics.

**Q: I found a bug in the framework?**
Please contact the TAs with steps to reproduce it.

**Q: On Windows, `-agent "me=python3 ..."` reports that python3 can't be found?**
On Windows the simulator launches your command with `cmd /C`, and Python is usually called `python`: write `-agent "me=python pysdk/examples/momentum.py"`.

**Q: Can I use languages other than Go for Part 2?**
Yes. The simulator passes the node address, private key, and contract addresses to your program through environment variables, so any language works (AGENTS.en.md documents the interface); Python and Go simply have ready-made SDKs. Go is recommended.

**For any other matters or questions not covered here, please consult the TAs promptly**.
