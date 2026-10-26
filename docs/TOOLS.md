# 用常见工具连接你的链

节点实现了标准以太坊 JSON-RPC（SPEC.md §12），下面的命令都在 devnet（`go run ./cmd/devnet`）运行时、已完成 Part 1 的链上验证过。默认第一个节点是 `http://127.0.0.1:28001`，chain id 4242。开发账户的私钥由 devnet 启动时打印（例如 alice 是 `0xcbf12da2...42c3`），**只在本地网络使用**。

## curl

```bash
RPC=http://127.0.0.1:28001
curl -s $RPC/status                                                     # 节点状态（管理接口）
curl -s -X POST $RPC -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}'
curl -s "$RPC/admin/chain?from=0&to=10"                                 # 规范链上的块
curl -s -X POST $RPC/admin/net -d '{"delay":"300ms","jitter":"300ms"}'  # 设置这个节点的网络延迟（整体替换：同时清掉 blocked）
```

## Foundry（cast / forge）

```bash
ALICE_KEY=<alice 的私钥>
cast block-number --rpc-url $RPC
cast balance 0xaf46011909C8480972eD90793b2FcDfb91223D6a --ether --rpc-url $RPC
cast send 0xaf46011909C8480972eD90793b2FcDfb91223D6a --value 1ether --private-key $ALICE_KEY --rpc-url $RPC

cast send <bob 的地址> --value 1ether --private-key $ALICE_KEY --rpc-url $RPC --async   # 立即返回哈希，不等回执
cast receipt <交易哈希> --rpc-url $RPC

(cd contracts && forge test -vv)                                 # 合约单元测试（test/DeFi.t.sol）
(cd contracts && forge create Token.sol:Token --broadcast --rpc-url $RPC --private-key $ALICE_KEY --constructor-args "Lab Coin" LAB)
cast call <代币地址> "symbol()(string)" --rpc-url $RPC
cast send <代币地址> "mint(address,uint256)" <地址> 1000000000000000000000 --private-key $ALICE_KEY --rpc-url $RPC
cast logs --from-block 0 --address <代币地址> --rpc-url $RPC
```

forge 第一次运行会下载 solc；离线时加 `--offline --use $(which solc)` 使用本机的 solc。会回滚的交易，cast 在估算 gas 时就会报出原因（例如 `execution reverted: balance`）。

## Python（web3.py）

```bash
pip install -r pysdk/requirements.txt
python3 pysdk/examples/basics.py $RPC     # 部署代币、铸币、转账、读事件、等待确认
```

自己写的脚本要注意两点：用 `get_transaction_count(addr, "pending")` 取 nonce（连续发多笔交易时 `"latest"` 会重复）；签名后的原始交易在 web3.py 7+ 是 `signed.raw_transaction`，在 6.x 是 `signed.rawTransaction`。

## JavaScript（ethers.js v6）

```bash
npm install ethers@6
node pysdk/examples/basics.mjs $RPC      # 部署、调用、查询事件、实时监听事件
```

ethers 用 `eth_newFilter` + `eth_getFilterChanges` 轮询事件，本链支持；`new JsonRpcProvider(url, undefined, {pollingInterval: 500})` 让它更快感知新块。

## MetaMask

1. 设置 → 网络 → 添加网络：RPC URL `http://127.0.0.1:28001`，Chain ID `4242`，货币符号 `ETH`。
2. 导入账户：粘贴 alice 的私钥。
3. 现在可以在 MetaMask 里转账，或者让网页 dapp 通过它和你的合约交互。

每次重启 devnet 都是一条新链：在 MetaMask 的"设置 → 高级 → 清除活动和 nonce 数据"里重置账户，否则它会用旧的 nonce。

## Go SDK（`client` 包）

```go
c := client.NewFromName("alice", "http://127.0.0.1:28001")   // 或 client.New(key, urls...)
c.Confirmations = 2                                           // Wait 等 2 个确认
token, _ := c.Deploy(ctx, client.TokenContract, "Lab Coin", "LAB")
c.Transact(ctx, client.TokenContract, token, "mint", c.Account, client.Ether(100))
bal, _ := c.ViewBig(ctx, client.TokenContract, token, "balanceOf", c.Account)
```

`Send` 先估算 gas（会回滚的交易直接返回带原因的 `*client.RevertError`），`Wait` 在交易因重组消失时继续等待并重新广播。

## 添加你自己的合约

1. 把 `.sol` 文件放进 `contracts/`；在 `contracts/test/` 下写 Foundry 测试（可选）。
2. Python / JS：`solc --abi --bin -o build/solc contracts/你的合约.sol`，或 `forge build`（产物在 `build/forge/`），然后用 ABI 和字节码部署。
3. Go：把合约名加到 `Makefile` 的 `CONTRACTS`，运行 `make contracts`（编译进 `client/artifacts/`），用 `client.Artifact("你的合约")` 加载。
