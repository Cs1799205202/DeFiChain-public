# defichain Protocol Specification (Reference)

This document is the precise specification for Part 1: if you implement according to the documentation, your node should be compatible with the specification; otherwise, during testing, they may reject each other's blocks. The tests (including hidden tests) only check the behavior documented here and in code comments.

## 1. Chain Parameters and Genesis Block

The chain parameters (`spec.ChainConfig`) are part of the genesis file, and all nodes on the same network must have identical ones:

| Parameter | devnet default | Meaning |
|---|---|---|
| `chainId` | 4242 | Written into every transaction's signature (EIP-155), preventing cross-chain replay |
| `targetBlockTime` | 2 (seconds) | The target block interval for difficulty adjustment, must be ≥ 2 |
| `difficultyBoundDivisor` | 16 | The step-size denominator for difficulty adjustment (Ethereum uses 2048) |
| `minimumDifficulty` | determined by hashrate | The minimum difficulty, must be ≥ `difficultyBoundDivisor` |
| `maxFutureTime` | 15 (seconds) | How far a block's timestamp may be ahead of the local clock |
| `gasLimit` | 30,000,000 | The gas cap per block (same for all blocks) |
| `blockReward` | 2 ETH | The amount rewarded to the coinbase per block |

The genesis file (`genesis.json`):

```json
{
  "config": { "chainId": 4242, "targetBlockTime": 2, "difficultyBoundDivisor": 16, "minimumDifficulty": 48750,
              "maxFutureTime": 15, "gasLimit": 30000000, "blockReward": "2000000000000000000" },
  "timestamp": 1760000000,
  "difficulty": 1560000,
  "extraData": "defichain devnet",
  "alloc": { "0xb4e6da1Cd324358Da7bC23DBc543D2Ec7015C848": { "balance": "10000000000000000000000" } }
}
```

Block 0: `ParentHash` = 0, `Number` = 0, `Coinbase` = 0, `Difficulty` = `difficulty`, `GasLimit` = `gasLimit`, `Time` = `timestamp`, `Extra` = the bytes of `extraData`, `BaseFee` = 0, `UncleHash` = the empty uncle hash, `TxHash`/`ReceiptHash` = the empty trie root, `Root` = the state root constructed from `alloc`. The total difficulty of the genesis block equals its own difficulty. The devnet tool sets `difficulty` (≈ 1.3 × total hashrate × target interval, so the network starts near steady state) and `minimumDifficulty` (`difficulty / 32`) according to the network's total hashrate.

## 2. Block Header

The block header is go-ethereum's `types.Header`; the fields and constraints used by this chain:

| Field | Constraint |
|---|---|
| `ParentHash` | The parent block's hash |
| `UncleHash` | Must be `types.EmptyUncleHash` (no uncles) |
| `Coinbase` | The miner's address, receiving the block reward and all transaction tips |
| `Root`, `TxHash`, `ReceiptHash`, `Bloom`, `GasUsed` | Commitments to the execution result, must match the result of re-execution (§6) |
| `Difficulty` | Must equal `CalcDifficulty` (§4) |
| `Number` | Parent block height + 1 |
| `GasLimit` | Must equal `gasLimit` |
| `Time` | Unix seconds, greater than the parent block's time, not exceeding local clock + `maxFutureTime` |
| `Extra` | At most 32 bytes, arbitrary content |
| `MixDigest`, `Nonce` | The PoW seal (§3) |
| `BaseFee` | Must be present and 0 (EIP-1559 base fee is disabled on this chain) |

Other fields (`WithdrawalsHash`, `BlobGasUsed`, etc.) are unused and nil. The block hash is the Keccak256 of the RLP encoding of the entire block header (go-ethereum's `Header.Hash()`).

## 3. PoW Seal

```
sealHash = Keccak256(RLP(header')), where header' is a copy of the header with Nonce = 0, MixDigest = 0
powHash  = Keccak256(sealHash ‖ nonce), nonce is 8 bytes big-endian (40 bytes of input in total)
target   = 2^256 / difficulty (integer division)
valid    ⇔  MixDigest == powHash  and  powHash (as a 256-bit big-endian integer) ≤ target
```

`RLP(header')` is go-ethereum's standard encoding of `types.Header` (`rlp.EncodeToBytes`), with field order: ParentHash, UncleHash, Coinbase, Root, TxHash, ReceiptHash, Bloom, Difficulty, Number, GasLimit, GasUsed, Time, Extra, MixDigest, Nonce, BaseFee (trailing optional fields are not encoded when nil).

**Test vector** (`labVectorHeader` in `chain/pow_test.go`): ParentHash = `0x11…11`, UncleHash = the empty uncle hash, Coinbase = `0x22…22`, Root = `0x33…33`, TxHash = ReceiptHash = the empty trie root, Bloom = 0, Difficulty = 131072, Number = 42, GasLimit = 30000000, GasUsed = 21000, Time = 1700000042, Extra = `"lab2"`, BaseFee = 0, MixDigest = `0x44…44`, Nonce = `0x0123456789abcdef`:

```
sealHash                    = 0x9b0a120487fb0dde8f9f81e4fe98960da85f7c1d704ccf9e245b36defa0857ad
powHash(sealHash, 0)        = 0xa770a6da07c0fa719824008e1d0dc6dc5cf15cba6df0d2a9e124edec49cc6ea1
powHash(sealHash, 1)        = 0x230a3251fd52f2b08d5c4a4b36c03425d5928585acc53cac91764b62d4194fa9
powHash(sealHash, 0x0123456789abcdef)
                            = 0xa6a5ef65797aa8dc47bc42543c78e596d074f8e69d4f34899c9d62874409b988
```

This is a simplified Hashcash, not Ethereum's Ethash (Ethash additionally uses a GB-scale dataset, making mining memory-bandwidth-bound). Consensus logic is independent of this.

**Hashrate throttling.** Mining calls `Throttle.Wait` once every `sealBatch` (1024) nonces tried, limiting the hashrate to the node's configured `hashrate` (per second, 0 means unlimited). This way several miners on one machine can have different hashrates without saturating the CPU.

## 4. Difficulty Adjustment

Ethereum Homestead rules (EIP-2), with this chain's parameters substituted:

```
Δ    = child.Time − parent.Time (if child.Time ≤ parent.Time, treat Δ as 0)
x    = max(1 − Δ / targetBlockTime, −99)              (integer division, rounded down)
diff = parent.Difficulty + parent.Difficulty / difficultyBoundDivisor × x   (integer division)
diff = max(diff, minimumDifficulty)
```

With the default parameters (2-second target, divisor 16): intervals of 0–1 second raise difficulty by +1/16; 2–3 seconds leave it unchanged; 4–5 seconds drop it by −1/16; slower intervals drop more, at most −99/16, but never below the minimum. Note that `x` can be very negative; `Δ / targetBlockTime` is compared against 100 before being converted to a signed number to avoid overflow.

Because timestamps are only accurate to the second, the steady-state average block interval under this rule is about 30% longer than the target (about 2.5 seconds when the target is 2 seconds), as was the case for Ethereum at the time. Block intervals follow an exponential distribution: the standard deviation is roughly equal to the mean.

## 5. Block Header Validation (`VerifyHeader`)

Given the parent block header and the local clock `now`, check in order; the first unsatisfied rule determines the returned error (judged with `errors.Is`):

| # | Rule | Error |
|---|---|---|
| 1 | `Number == parent.Number + 1` | `ErrInvalidNumber` |
| 2 | `Time > parent.Time` | `ErrInvalidTimestamp` |
| 3 | `Time ≤ now + maxFutureTime` | `ErrFutureBlock` |
| 4 | `Difficulty == CalcDifficulty(cfg, Time, parent)` | `ErrInvalidDifficulty` |
| 5 | `GasLimit == gasLimit` | `ErrInvalidGasLimit` |
| 6 | `GasUsed ≤ GasLimit` | `ErrInvalidGasUsed` |
| 7 | `len(Extra) ≤ 32` | `ErrExtraTooLong` |
| 8 | `UncleHash == EmptyUncleHash` | `ErrInvalidUncles` |
| 9 | `BaseFee` present and 0 | `ErrInvalidBaseFee` |
| 10 | Seal is valid (§3): difficulty ≤ 0 or missing, `MixDigest` mismatch, hash exceeds the target | `ErrInvalidDifficulty`, `ErrInvalidMixDigest`, `ErrInvalidPoW` |

`ParentHash` is not checked here: the caller uses it to find the parent block in the first place. Rule 3 depends on the local clock, so a "future" block is rejected now but may become valid a little later; the node does not store it (it cannot be treated as permanently illegal) and will receive it again during later synchronization.

## 6. Block Execution and Validation (Task 3)

**Execution rules.** The EVM enables all Ethereum upgrades up to Prague (the default compile target for Solidity 0.8.24+), and the base fee is always 0, so each transaction pays `gasUsed × tip` to the coinbase (done in `applyTx`). `PREVRANDAO` (Solidity's `block.prevrandao`) is `Keccak256(parentHash)`: predictable, and **must not be used as a random number in contracts**. `BLOCKHASH` returns the ancestors of the block itself (when executed on a side chain, the ancestors of the side chain).

**Executing a block**: on the parent block's state, with a gas pool of capacity `gasLimit`, call `applyTx` for each transaction in order (the index of the i-th transaction is i); after all have executed, add `blockReward` to the coinbase (`payReward`).

A transaction that returns an error in `applyTx` is **illegal** (unsupported type, wrong chain id, invalid signature, nonce not the account's next one, balance cannot cover `gas × feeCap + value`, gas below intrinsic cost, or the gas pool cannot fit its `gas`), and cannot appear in a block. A transaction that reverts or runs out of gas during execution is **legal**: it is in the block, has a receipt (`status = 0`), paid the fee, and its nonce increments; its revert reason is recorded in `BlockResult.Reverts` (the `revertReason` field of the JSON-RPC receipt).

**Validation (`processBlock`)**, failing any of the following returns an error wrapping `ErrInvalidBlock`:

1. The block body has no uncles and no withdrawals (`Withdrawals() == nil`);
2. `TxHash == txsRoot(transaction list)`;
3. Execute per the rules above; **if any transaction is illegal, the entire block is illegal** (you cannot skip it);
4. `GasUsed` (the gas pool's cumulative usage), `Bloom`, `ReceiptHash`, and `Root` all match the execution result.

Only after all pass is the new state committed (`commitState`), returning each transaction's receipt and revert reason (in block order). A rejected block leaves no state behind. The `BlockHash` of receipts and logs is this block's hash.

**Building a block (`BuildBlock`)** uses the same rules, but illegal transactions are **skipped in place**: the remaining transactions are still tried one by one in the given order, so a transaction that does not fit does not block later, smaller transactions. It then pays the reward, fills in `Root`, `GasUsed`, `TxHash`, `ReceiptHash`, `Bloom`, `UncleHash` (empty), and does not write the state database.

**Test vector.** On the test genesis block (`labGenesis`: alice, bob, carol, deployer each have 1000 ETH), miner `0x…aa`'s block 1 successively contains alice→bob 10 ETH (nonce 0, tip 2 gwei), bob→carol 5 ETH (nonce 0, tip 1 gwei), alice→carol 1 ETH (nonce 1, tip 3 gwei), all 21000-gas EIP-1559 transactions: `GasUsed = 63000`, `Root = 0x44c33873bf842924e79f689874bb7b49d417225b899c5b489d95a62acea267cc`, `ReceiptHash = 0x25e6b7af647c519a27cc13276a1e6abc46154b51414d174b072698df1f6c19df` (see `TestExecStateRootVector`).

## 7. Transaction Pool (Task 4)

**Admission (`Add`)**, in order:

1. `ValidateTx` (state-independent): only accepts legacy transactions with EIP-155 replay protection, EIP-2930, and EIP-1559 (no blob transactions or EIP-7702); the chain id is this chain's; the signature is valid; `gas ≤ gasLimit`; `gas` is not below the intrinsic cost (21000 + call data + contract creation + access list); `feeCap ≥ tip`;
2. The transaction is already in the pool (same hash): return `(false, nil)`;
3. Against the chain head state: `nonce < account nonce` → `core.ErrNonceTooLow`; `balance < gas × feeCap + value` → `core.ErrInsufficientFunds`;
4. The pool already has a transaction from the same sender with the same nonce: the new transaction's tip (`GasTipCap`) must be **strictly higher** to replace it, otherwise `ErrReplaceUnderpriced`;
5. The pool already has `MaxPoolTxs` (8192, pending plus queued combined) transactions and the new one is not a replacement: `ErrPoolFull`.

Transactions with gaps in nonce are also accepted (queued).

**pending and queued.** For each sender, the contiguous run of nonces starting from its nonce on the chain head state is pending, and the rest is queued. `PendingNonce(addr) = account nonce + length of the pending run`. `Content()` lists pending and queued separately per sender (each sorted by nonce; senders with no transactions of that kind do not appear).

**Ordering (`Pending()`).** Returns all pending transactions: within each sender, sorted by nonce; across senders, at each step pick the one with the highest tip among "each sender's next transaction" (the tip of a legacy transaction is its gas price), and on a tie take the one that entered the pool first. This is a miner's strategy, not a consensus rule, but the tests and Part 2's simulation depend on it.

**Chain head changes (`reset`).** After `setHead` changes the chain head each time, it calls `reset(new head's state, dropped)`, where `dropped` are the transactions from blocks that left the canonical chain, older blocks first. First, the transactions in `dropped` are re-added one by one per the admission rules above (but against the new state); then all transactions with `nonce < the sender's nonce on the new state` are removed. It returns the re-added transactions, which the node rebroadcasts to its peers.

All methods of the transaction pool may be called concurrently by multiple goroutines.

## 8. Fork Choice and Reorg

- The node stores every valid block it has seen, along with its total difficulty `TD(b) = TD(parent) + b.Difficulty`.
- The chain head is the block with the greatest total difficulty. **Switch only when the new block's total difficulty is strictly greater than the current head's**; on a tie, keep the current head (first come, first served).
- The canonical chain index (height → hash) must be exactly the path from the genesis block to the head: heights above the head have no canonical block. The transaction index (tx hash → location) contains only transactions in canonical blocks. JSON-RPC reads only these two indexes, so if they are wrong, wallets and Agents will see the wrong chain.
- On reorg, transactions from the old branch (after the common ancestor, not on the new branch) are handed to `setHead` to be put back into the transaction pool.

## 9. P2P Protocol

Nodes communicate over HTTP (`chain/p2p.go`). Each request carries `X-Node-Id` (the sender) and `X-Genesis` (the genesis block hash; requests from different networks are rejected).

| Message | Description |
|---|---|
| `POST /p2p/block` | Push a block (RLP). The receiver hands each one to `Handler.HandleBlock(block, from)` in arrival order |
| `POST /p2p/tx` | Push a transaction. The receiver adds it to the transaction pool and, if new, forwards it to other peers |
| `GET /p2p/head` | Returns `{number, hash, td}`: the sender's canonical chain head |
| `GET /p2p/ancestors?hash=H&max=N` | Returns block H and its ancestors, newest to oldest, up to N (≤ `MaxAncestors` = 256) blocks, excluding the genesis block |

Fault injection (`/admin/net`) acts on the P2P layer: peers in `blocked` receive no messages from this node, and their requests are also rejected (so configuring one side is a bidirectional disconnect); `delay` and `jitter` are added to every message this node sends; `drop` is the probability that pushes (blocks, transactions) are dropped; requests are not dropped.

## 10. Synchronization

- **Push**: mined blocks are broadcast to all peers; a received valid new block (canonical or side chain) is forwarded to all peers except the sender; known and illegal blocks are not forwarded.
- **Fill**: on receiving a block whose parent is unknown, request backwards from the sender via `/p2p/ancestors` until it connects to a locally held block, then insert from oldest to newest. Check that the returned blocks are contiguous.
- **Pull**: at least every 2 seconds (`SyncInterval` = 1 second), query all peers' chain heads; for heads with total difficulty greater than the local one and not present locally, pull the chain from the peer with the greatest total difficulty; on failure try the next one. After a partition heals, even without new blocks, nodes must converge within a few seconds.
- **Startup**: the node does not persist to disk; every start begins from the genesis block. The framework starts P2P first, calls `SyncOnce` once (waiting up to 15 seconds), then starts the pull loop, and finally (if configured) starts the miner.

## 11. Node Configuration and Admin Interface

Node configuration (`spec.NodeConfig`, `go run ./cmd/node -genesis genesis.json -config n1.json`): `id`, `listen` (must be a local address unless `unsafeExpose` is set), `peers` (id → URL, may connect to only some nodes), `coinbase`, `mine`, `hashrate`, `net` (initial fault injection). The devnet tool generates these files for you.

Admin interface (same port as JSON-RPC; rejects write requests from web pages):

| Interface | Description |
|---|---|
| `GET /status` | id, genesis block, chain head (height, hash, total difficulty), peers, mining status, hashrate, transaction pool size, reorg count, number of stored blocks, fault injection settings |
| `POST /admin/mine {"blocks": n}` | Mine n blocks immediately (automatic mining must be off) |
| `GET/POST /admin/miner {"mining": bool, "hashrate": h}` | Toggle automatic mining and set the hashrate |
| `GET/POST /admin/net {"blocked": [...], "delay": "200ms", "jitter": "100ms", "drop": 0.1}` | Fault injection (full replacement) |
| `POST /admin/peers {"add": {"n5": "http://..."}, "remove": ["n4"]}` | Add and remove peers |
| `GET /admin/reorgs` | Reorg count and the most recent reorg: old head, new head, common ancestor, number of blocks removed/added, number of transactions returned to the pool |
| `GET /admin/chain?from=&to=` | Blocks on the canonical chain (default: the most recent 50) |
| `GET /admin/blocks?from=` | All stored blocks (the entire tree), including whether canonical |

## 12. JSON-RPC

`POST /` or `/rpc`, supports batch requests and cross-origin access (the node stores no private keys, so a web page can do nothing through it that it could not do itself). Supports: `eth_chainId`, `net_version`, `eth_blockNumber`, `eth_getBalance`, `eth_getCode`, `eth_getStorageAt`, `eth_getTransactionCount` (`pending` includes the transaction pool), `eth_call`, `eth_estimateGas`, `eth_sendRawTransaction`, `eth_getBlockByNumber/ByHash` (includes `totalDifficulty`), `eth_getTransactionByHash` (also checks the transaction pool), `eth_getTransactionReceipt`, `eth_getBlockReceipts`, `eth_getLogs`, `eth_newFilter`/`eth_newBlockFilter`/`eth_newPendingTransactionFilter`/`eth_getFilterChanges`/`eth_getFilterLogs`/`eth_uninstallFilter`, `eth_gasPrice`, `eth_maxPriorityFeePerGas`, `eth_feeHistory`, `txpool_status`, `txpool_content`, etc. There is no WebSocket subscription; clients poll via filters (all common libraries support this).

Differences from Ethereum mainnet: the block tag `safe` means 6 blocks below the head, and `finalized` means 12 blocks below the head (PoW has no real finality; this is just a confirmation-count convention); receipts have an extra `revertReason` field (the reason for a failed transaction); on reorg, log filters report the logs of removed blocks once more with `"removed": true` (as geth does).

## 13. Differences from Bitcoin and Ethereum

| | Bitcoin | Ethereum (PoW era) | This lab |
|---|---|---|---|
| PoW function | double SHA-256 | Ethash (memory-hard) | Keccak256 Hashcash |
| Block interval | 10 minutes | ~13 seconds | ~2.5 seconds |
| Difficulty adjustment | every 2016 blocks | per block (Homestead rule) | per block (Homestead rule, different parameters) |
| Fork choice | greatest cumulative work | greatest total difficulty (including uncles) | greatest total difficulty, no uncles |
| State model | UTXO | accounts + EVM | accounts + EVM (Prague) |
| Node communication | custom TCP protocol | devp2p | HTTP (easy to observe and inject faults) |
| Persistence | yes | yes | no (syncs from peers after restart) |
