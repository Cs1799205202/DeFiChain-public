# defichain 协议规范（参考）

本文是 Part 1 的精确规范：如果按照文档实现，你的节点应当和规范兼容，否则测试时可能会发生它们互相不接受对方的区块的问题。测试（包括隐藏测试）只检查本文和代码注释里写明的行为。

## 1. 链参数与创世块

链参数（`spec.ChainConfig`）是创世文件的一部分，同一个网络的所有节点必须相同：

| 参数 | devnet 默认值 | 含义 |
|---|---|---|
| `chainId` | 4242 | 写进每笔交易的签名（EIP-155），防止跨链重放 |
| `targetBlockTime` | 2（秒） | 难度调整的目标出块间隔，必须 ≥ 2 |
| `difficultyBoundDivisor` | 16 | 难度调整的步长分母（以太坊是 2048） |
| `minimumDifficulty` | 由算力决定 | 难度下限，必须 ≥ `difficultyBoundDivisor` |
| `maxFutureTime` | 15（秒） | 区块时间戳最多比本地时钟超前多少 |
| `gasLimit` | 30,000,000 | 每个块的 gas 上限（所有块相同） |
| `blockReward` | 2 ETH | 每个块奖励给 coinbase 的金额 |

创世文件（`genesis.json`）：

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

0 号区块：`ParentHash` = 0，`Number` = 0，`Coinbase` = 0，`Difficulty` = `difficulty`，`GasLimit` = `gasLimit`，`Time` = `timestamp`，`Extra` = `extraData` 的字节，`BaseFee` = 0，`UncleHash` = 空叔块哈希，`TxHash`/`ReceiptHash` = 空 trie 根，`Root` = 按 `alloc` 构造的状态根。创世块的总难度等于它自己的难度。devnet 工具按全网算力设置 `difficulty`（≈ 1.3 × 总算力 × 目标间隔，使网络一开始就接近稳态）和 `minimumDifficulty`（`difficulty / 32`）。

## 2. 区块头

区块头是 go-ethereum 的 `types.Header`，本链用到的字段与约束：

| 字段 | 约束 |
|---|---|
| `ParentHash` | 父块的哈希 |
| `UncleHash` | 必须是 `types.EmptyUncleHash`（没有叔块） |
| `Coinbase` | 矿工地址，获得区块奖励和所有交易的小费 |
| `Root`、`TxHash`、`ReceiptHash`、`Bloom`、`GasUsed` | 执行结果的承诺，必须与重新执行的结果一致（§6） |
| `Difficulty` | 必须等于 `CalcDifficulty`（§4） |
| `Number` | 父块高度 + 1 |
| `GasLimit` | 必须等于 `gasLimit` |
| `Time` | Unix 秒，大于父块时间，不超过本地时钟 + `maxFutureTime` |
| `Extra` | 最多 32 字节，内容随意 |
| `MixDigest`、`Nonce` | PoW 封印（§3） |
| `BaseFee` | 必须存在且为 0（EIP-1559 的基础费用在本链关闭） |

其余字段（`WithdrawalsHash`、`BlobGasUsed` 等）不使用，为 nil。区块的哈希是整个区块头 RLP 编码的 Keccak256（go-ethereum 的 `Header.Hash()`）。

## 3. PoW 封印

```
sealHash = Keccak256(RLP(header'))，header' 是区块头的副本，其中 Nonce = 0、MixDigest = 0
powHash  = Keccak256(sealHash ‖ nonce)，nonce 是 8 字节大端序（共 40 字节输入）
target   = 2^256 / difficulty（整数除法）
合法     ⇔  MixDigest == powHash  且  powHash（作为 256 位大端整数）≤ target
```

`RLP(header')` 就是 go-ethereum 对 `types.Header` 的标准编码（`rlp.EncodeToBytes`），字段顺序为：ParentHash、UncleHash、Coinbase、Root、TxHash、ReceiptHash、Bloom、Difficulty、Number、GasLimit、GasUsed、Time、Extra、MixDigest、Nonce、BaseFee（后面的可选字段为 nil 时不编码）。

**测试向量**（`chain/pow_test.go` 里的 `labVectorHeader`）：ParentHash = `0x11…11`，UncleHash = 空叔块哈希，Coinbase = `0x22…22`，Root = `0x33…33`，TxHash = ReceiptHash = 空 trie 根，Bloom = 0，Difficulty = 131072，Number = 42，GasLimit = 30000000，GasUsed = 21000，Time = 1700000042，Extra = `"lab2"`，BaseFee = 0，MixDigest = `0x44…44`，Nonce = `0x0123456789abcdef`：

```
sealHash                    = 0x9b0a120487fb0dde8f9f81e4fe98960da85f7c1d704ccf9e245b36defa0857ad
powHash(sealHash, 0)        = 0xa770a6da07c0fa719824008e1d0dc6dc5cf15cba6df0d2a9e124edec49cc6ea1
powHash(sealHash, 1)        = 0x230a3251fd52f2b08d5c4a4b36c03425d5928585acc53cac91764b62d4194fa9
powHash(sealHash, 0x0123456789abcdef)
                            = 0xa6a5ef65797aa8dc47bc42543c78e596d074f8e69d4f34899c9d62874409b988
```

这是简化的 Hashcash，不是以太坊的 Ethash（Ethash 额外使用一个 GB 级的数据集，让挖矿受内存带宽限制）。共识逻辑与之无关。

**算力限制。** 挖矿每尝试 `sealBatch`（1024）个 nonce 调用一次 `Throttle.Wait`，把算力限制在节点配置的 `hashrate`（次/秒，0 为不限制）。这样一台电脑上的几个矿工可以有不同的算力，也不会占满 CPU。

## 4. 难度调整

以太坊 Homestead 的规则（EIP-2），参数换成本链的：

```
Δ    = child.Time − parent.Time（若 child.Time ≤ parent.Time，按 Δ = 0 计算）
x    = max(1 − Δ / targetBlockTime, −99)              （整数除法，向下取整）
diff = parent.Difficulty + parent.Difficulty / difficultyBoundDivisor × x   （整数除法）
diff = max(diff, minimumDifficulty)
```

以默认参数（目标 2 秒、除数 16）为例：间隔 0–1 秒，难度 +1/16；2–3 秒，不变；4–5 秒，−1/16；更慢则下调得更多，最多 −99/16，但不低于下限。注意 `x` 可能很负，`Δ / targetBlockTime` 先与 100 比较再转成有符号数，避免溢出。

由于时间戳只精确到秒，这个规则的稳态平均出块间隔比目标值长约 30%（目标 2 秒时约 2.5 秒），以太坊当年也是如此。出块间隔服从指数分布：标准差约等于均值。

## 5. 区块头验证（`VerifyHeader`）

给定父块头和本地时钟 `now`，按顺序检查，第一条不满足的规则决定返回的错误（用 `errors.Is` 判断）：

| # | 规则 | 错误 |
|---|---|---|
| 1 | `Number == parent.Number + 1` | `ErrInvalidNumber` |
| 2 | `Time > parent.Time` | `ErrInvalidTimestamp` |
| 3 | `Time ≤ now + maxFutureTime` | `ErrFutureBlock` |
| 4 | `Difficulty == CalcDifficulty(cfg, Time, parent)` | `ErrInvalidDifficulty` |
| 5 | `GasLimit == gasLimit` | `ErrInvalidGasLimit` |
| 6 | `GasUsed ≤ GasLimit` | `ErrInvalidGasUsed` |
| 7 | `len(Extra) ≤ 32` | `ErrExtraTooLong` |
| 8 | `UncleHash == EmptyUncleHash` | `ErrInvalidUncles` |
| 9 | `BaseFee` 存在且为 0 | `ErrInvalidBaseFee` |
| 10 | 封印合法（§3）：难度 ≤ 0 或缺失、`MixDigest` 不符、哈希超过目标值 | `ErrInvalidDifficulty`、`ErrInvalidMixDigest`、`ErrInvalidPoW` |

`ParentHash` 不在这里检查：调用者就是用它找到父块的。规则 3 依赖本地时钟，所以"未来"的块现在被拒绝，过一会儿可能就合法了：节点不存储它（不能把它当作永久非法），以后同步时会再次收到。

## 6. 区块的执行与验证（任务 3）

**执行规则。** EVM 启用以太坊直到 Prague 的所有升级（Solidity 0.8.24+ 默认编译目标），基础费用恒为 0，所以每笔交易支付 `gasUsed × 小费` 给 coinbase（`applyTx` 里完成）。`PREVRANDAO`（Solidity 的 `block.prevrandao`）是 `Keccak256(parentHash)`：可以预测，**不能在合约里当随机数用**。`BLOCKHASH` 返回的是该区块自己的祖先（在侧链上执行时就是侧链的祖先）。

**执行一个块**：在父块的状态上，用一个容量为 `gasLimit` 的 gas 池，按顺序对每笔交易调用 `applyTx`（第 i 笔交易的 index 是 i）；全部执行完后给 coinbase 加 `blockReward`（`payReward`）。

一笔交易在 `applyTx` 里返回错误，就是**非法**的（类型不支持、chain id 不对、签名无效、nonce 不是账户的下一个、余额付不起 `gas × feeCap + value`、gas 低于内在消耗、gas 池剩下的装不下它的 `gas`），不能出现在区块里。执行时 revert 或 gas 耗尽的交易是**合法**的：它在区块里，有回执（`status = 0`），付了手续费，nonce 加一；它的 revert 原因记在 `BlockResult.Reverts`（JSON-RPC 回执的 `revertReason` 字段）。

**验证（`processBlock`）**，任何一项不满足都返回包装了 `ErrInvalidBlock` 的错误：

1. 区块体里没有叔块，没有提款（`Withdrawals() == nil`）；
2. `TxHash == txsRoot(交易列表)`；
3. 按上面的规则执行，**任何一笔交易非法，整个块非法**（不能跳过它）；
4. `GasUsed`（gas 池的累计用量）、`Bloom`、`ReceiptHash`、`Root` 都与执行结果一致。

全部通过后才提交新状态（`commitState`），返回每笔交易的回执和 revert 原因（按区块中的顺序）。被拒绝的块不留下任何状态。回执的 `BlockHash` 和日志的 `BlockHash` 是这个块的哈希。

**造块（`BuildBlock`）** 用同样的规则，但非法交易被**原地跳过**：其余交易仍按给定顺序逐个尝试，所以一笔装不下的交易不妨碍后面更小的交易。然后发奖励，填好 `Root`、`GasUsed`、`TxHash`、`ReceiptHash`、`Bloom`、`UncleHash`（空），不写状态数据库。

**测试向量。** 测试用的创世块（`labGenesis`：alice、bob、carol、deployer 各 1000 ETH）上，矿工 `0x…aa` 的 1 号块依次包含 alice→bob 10 ETH（nonce 0，小费 2 gwei）、bob→carol 5 ETH（nonce 0，小费 1 gwei）、alice→carol 1 ETH（nonce 1，小费 3 gwei），都是 21000 gas 的 EIP-1559 交易：`GasUsed = 63000`，`Root = 0x44c33873bf842924e79f689874bb7b49d417225b899c5b489d95a62acea267cc`，`ReceiptHash = 0x25e6b7af647c519a27cc13276a1e6abc46154b51414d174b072698df1f6c19df`（见 `TestExecStateRootVector`）。

## 7. 交易池（任务 4）

**准入（`Add`）**，按顺序：

1. `ValidateTx`（与状态无关）：只接受带 EIP-155 重放保护的 legacy 交易、EIP-2930、EIP-1559（不接受 blob 交易和 EIP-7702）；chain id 是本链的；签名有效；`gas ≤ gasLimit`；`gas` 不低于内在消耗（21000 + 调用数据 + 合约创建 + 访问列表）；`feeCap ≥ tip`；
2. 交易已经在池里（同一哈希）：返回 `(false, nil)`；
3. 对照链头状态：`nonce < 账户 nonce` → `core.ErrNonceTooLow`；`余额 < gas × feeCap + value` → `core.ErrInsufficientFunds`；
4. 池里已有同一发送者、同一 nonce 的交易：新交易的 tip（`GasTipCap`）**严格更高**才替换它，否则 `ErrReplaceUnderpriced`；
5. 池里已有 `MaxPoolTxs`（8192，pending 和 queued 合计）笔交易、且新交易不是替换：`ErrPoolFull`。

nonce 有空缺的交易也接受（queued）。

**pending 与 queued。** 对每个发送者，从它在链头状态上的 nonce 开始、nonce 连续的那一段是 pending，其余是 queued。`PendingNonce(addr) = 账户 nonce + pending 段的长度`。`Content()` 按发送者分别列出 pending 和 queued（各自按 nonce 排序；没有该类交易的发送者不出现）。

**顺序（`Pending()`）。** 返回所有 pending 交易：每个发送者内部按 nonce；发送者之间，每一步取"各发送者的下一笔交易"中 tip 最高的（legacy 交易的 tip 是它的 gas price），tip 相同时取先进入交易池的。这是矿工的策略而不是共识规则，但测试和 Part 2 的仿真依赖它。

**链头变化（`reset`）。** `setHead` 每次改变链头后调用 `reset(新链头的状态, dropped)`，`dropped` 是离开规范链的块里的交易，旧块在前。先把 `dropped` 里的交易按上面的准入规则（但对照新状态）逐个放回，再删除所有 `nonce < 发送者在新状态上的 nonce` 的交易。返回放回去的交易，节点会把它们重新广播给对等节点。

交易池的所有方法都可能被多个 goroutine 同时调用。

## 8. 分叉选择与重组

- 节点存储它见过的每一个合法区块，以及它的总难度 `TD(b) = TD(parent) + b.Difficulty`。
- 链头是总难度最大的区块。**新块的总难度严格大于当前链头时才切换**；相等时保留当前链头（先到先得）。
- 规范链索引（高度 → 哈希）必须恰好是从创世块到链头的路径：链头以上的高度没有规范块。交易索引（交易哈希 → 位置）只包含规范块里的交易。JSON-RPC 只读这两个索引，所以索引错了，钱包和 Agent 就会看到错误的链。
- 重组时，旧分支（公共祖先之后、不在新分支上）的交易交给 `setHead` 放回交易池。

## 9. P2P 协议

节点之间通过 HTTP 通信（`chain/p2p.go`）。每个请求带 `X-Node-Id`（发送者）和 `X-Genesis`（创世块哈希，不同网络的请求被拒绝）。

| 消息 | 说明 |
|---|---|
| `POST /p2p/block` | 推送一个区块（RLP）。接收方按到达顺序逐个交给 `Handler.HandleBlock(block, from)` |
| `POST /p2p/tx` | 推送一笔交易。接收方加入交易池，若是新的则转发给其他对等节点 |
| `GET /p2p/head` | 返回 `{number, hash, td}`：发送者的规范链头 |
| `GET /p2p/ancestors?hash=H&max=N` | 返回区块 H 及其祖先，从新到旧，最多 N（≤ `MaxAncestors` = 256）个，不含创世块 |

故障注入（`/admin/net`）作用于 P2P 层：`blocked` 里的对等节点收不到这个节点的任何消息，它们的请求也被拒绝（所以一边配置就是双向断开）；`delay`、`jitter` 加在这个节点发出的每条消息上；`drop` 是推送（区块、交易）被丢弃的概率，请求不丢。

## 10. 同步

- **推送**：挖出的块广播给所有对等节点；收到一个合法的新块（规范或侧链）转发给除发送者外的所有对等节点；已知的块和非法的块不转发。
- **补齐**：收到父块未知的块，向发送者用 `/p2p/ancestors` 往回要，直到接上本地已有的块，然后从旧到新插入。检查返回的块是否首尾相接。
- **拉取**：至少每 2 秒（`SyncInterval` = 1 秒）询问所有对等节点的链头；对总难度大于自己、且本地没有的链头，从总难度最大的对等节点拉链；失败则换下一个。分区愈合后，即使没有新块，节点也要在几秒内收敛。
- **启动**：节点不落盘，每次启动都从创世块开始。框架先启动 P2P，调用一次 `SyncOnce`（最多等 15 秒），再启动拉取循环，最后（若配置了）启动矿工。

## 11. 节点配置与管理接口

节点配置（`spec.NodeConfig`，`go run ./cmd/node -genesis genesis.json -config n1.json`）：`id`、`listen`（必须是本机地址，除非设置 `unsafeExpose`）、`peers`（id → URL，可以只连部分节点）、`coinbase`、`mine`、`hashrate`、`net`（初始故障注入）。devnet 工具会为你生成这些文件。

管理接口（与 JSON-RPC 同一端口；拒绝来自网页的写请求）：

| 接口 | 说明 |
|---|---|
| `GET /status` | id、创世块、链头（高度、哈希、总难度）、对等节点、挖矿状态、算力、交易池大小、重组次数、存储的区块数、故障注入设置 |
| `POST /admin/mine {"blocks": n}` | 立即挖 n 个块（自动挖矿必须关闭） |
| `GET/POST /admin/miner {"mining": bool, "hashrate": h}` | 开关自动挖矿、设置算力 |
| `GET/POST /admin/net {"blocked": [...], "delay": "200ms", "jitter": "100ms", "drop": 0.1}` | 故障注入（整体替换） |
| `POST /admin/peers {"add": {"n5": "http://..."}, "remove": ["n4"]}` | 增删对等节点 |
| `GET /admin/reorgs` | 重组次数和最近的重组：旧链头、新链头、公共祖先、移除/加入的块数、放回交易池的交易数 |
| `GET /admin/chain?from=&to=` | 规范链上的区块（默认最近 50 个） |
| `GET /admin/blocks?from=` | 存储的所有区块（整棵树），含是否规范 |

## 12. JSON-RPC

`POST /` 或 `/rpc`，支持批量请求，允许跨域（节点不保存任何私钥，网页通过它做不了自己做不到的事）。支持：`eth_chainId`、`net_version`、`eth_blockNumber`、`eth_getBalance`、`eth_getCode`、`eth_getStorageAt`、`eth_getTransactionCount`（`pending` 包含交易池）、`eth_call`、`eth_estimateGas`、`eth_sendRawTransaction`、`eth_getBlockByNumber/ByHash`（含 `totalDifficulty`）、`eth_getTransactionByHash`（也查交易池）、`eth_getTransactionReceipt`、`eth_getBlockReceipts`、`eth_getLogs`、`eth_newFilter`/`eth_newBlockFilter`/`eth_newPendingTransactionFilter`/`eth_getFilterChanges`/`eth_getFilterLogs`/`eth_uninstallFilter`、`eth_gasPrice`、`eth_maxPriorityFeePerGas`、`eth_feeHistory`、`txpool_status`、`txpool_content` 等。没有 WebSocket 订阅，客户端用过滤器轮询（所有常用库都支持）。

与以太坊主网的区别：区块标签 `safe` 表示链头往下 6 个块，`finalized` 表示往下 12 个块（PoW 没有真正的终局，这只是一个确认数约定）；回执多一个字段 `revertReason`（失败交易的原因）；日志过滤器在重组时会把被移除区块的日志以 `"removed": true` 再报告一次（和 geth 一样）。

## 13. 与比特币、以太坊的差异

| | 比特币 | 以太坊（PoW 时期） | 本实验 |
|---|---|---|---|
| PoW 函数 | 双 SHA-256 | Ethash（内存困难） | Keccak256 Hashcash |
| 出块间隔 | 10 分钟 | 约 13 秒 | 约 2.5 秒 |
| 难度调整 | 每 2016 块 | 每块（Homestead 规则） | 每块（Homestead 规则，参数不同） |
| 分叉选择 | 累计工作量最大 | 总难度最大（含叔块） | 总难度最大，无叔块 |
| 状态模型 | UTXO | 账户 + EVM | 账户 + EVM（Prague） |
| 节点通信 | TCP 自定义协议 | devp2p | HTTP（便于观察和注入故障） |
| 持久化 | 有 | 有 | 无（重启后从对等节点同步） |
