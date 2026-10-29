# Connect to your chain with common tools

The node implements standard Ethereum JSON-RPC (SPEC.en.md §12). All commands below have been verified on a devnet (`go run ./cmd/devnet`) runtime, on a chain that has completed Part 1. The default first node is `http://127.0.0.1:28001`, chain id 4242. The dev account's private key is printed when devnet starts (for example, alice's is `0xcbf12da2...42c3`), and should be **used only on the local network**.

## curl

```bash
RPC=http://127.0.0.1:28001
curl -s $RPC/status                                                     # node status (admin interface)
curl -s -X POST $RPC -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}'
curl -s "$RPC/admin/chain?from=0&to=10"                                 # blocks on the canonical chain
curl -s -X POST $RPC/admin/net -d '{"delay":"300ms","jitter":"300ms"}'  # set this node's network latency (full replacement: also clears blocked)
```

## Foundry (cast / forge)

```bash
ALICE_KEY=<alice's private key>
cast block-number --rpc-url $RPC
cast balance 0xaf46011909C8480972eD90793b2FcDfb91223D6a --ether --rpc-url $RPC
cast send 0xaf46011909C8480972eD90793b2FcDfb91223D6a --value 1ether --private-key $ALICE_KEY --rpc-url $RPC

cast send <bob's address> --value 1ether --private-key $ALICE_KEY --rpc-url $RPC --async   # returns hash immediately, doesn't wait for receipt
cast receipt <transaction hash> --rpc-url $RPC

(cd contracts && forge test -vv)                                 # contract unit tests (test/DeFi.t.sol)
(cd contracts && forge create Token.sol:Token --broadcast --rpc-url $RPC --private-key $ALICE_KEY --constructor-args "Lab Coin" LAB)
cast call <token address> "symbol()(string)" --rpc-url $RPC
cast send <token address> "mint(address,uint256)" <address> 1000000000000000000000 --private-key $ALICE_KEY --rpc-url $RPC
cast logs --from-block 0 --address <token address> --rpc-url $RPC
```

The first time forge runs it downloads solc; when offline, add `--offline --use $(which solc)` to use the local solc. For transactions that will revert, cast reports the reason during gas estimation (for example `execution reverted: balance`).

## Python (web3.py)

```bash
pip install -r pysdk/requirements.txt
python3 pysdk/examples/basics.py $RPC     # deploy token, mint, transfer, read events, wait for confirmation
```

Two things to watch out for in your own scripts: use `get_transaction_count(addr, "pending")` to get the nonce (with `"latest"` it will repeat when sending multiple transactions in a row); the raw signed transaction is `signed.raw_transaction` in web3.py 7+, and `signed.rawTransaction` in 6.x.

## JavaScript (ethers.js v6)

```bash
npm install ethers@6
node pysdk/examples/basics.mjs $RPC      # deploy, call, query events, listen to events in real time
```

ethers polls events with `eth_newFilter` + `eth_getFilterChanges`, which this chain supports; `new JsonRpcProvider(url, undefined, {pollingInterval: 500})` makes it notice new blocks faster.

## MetaMask

1. Settings → Networks → Add Network: RPC URL `http://127.0.0.1:28001`, Chain ID `4242`, Currency Symbol `ETH`.
2. Import account: paste alice's private key.
3. You can now transfer in MetaMask, or let a web dapp interact with your contracts through it.

Every devnet restart is a new chain: reset the account in MetaMask under "Settings → Advanced → Clear activity and nonce data", otherwise it will use stale nonces.

## Go SDK (`client` package)

```go
c := client.NewFromName("alice", "http://127.0.0.1:28001")   // or client.New(key, urls...)
c.Confirmations = 2                                           // Wait waits for 2 confirmations
token, _ := c.Deploy(ctx, client.TokenContract, "Lab Coin", "LAB")
c.Transact(ctx, client.TokenContract, token, "mint", c.Account, client.Ether(100))
bal, _ := c.ViewBig(ctx, client.TokenContract, token, "balanceOf", c.Account)
```

`Send` estimates gas first (a reverting transaction returns a `*client.RevertError` with the reason), and `Wait` keeps waiting and re-broadcasting if a transaction disappears due to a reorg.

## Add your own contracts

1. Put the `.sol` file in `contracts/`; write Foundry tests under `contracts/test/` (optional).
2. Python / JS: `solc --abi --bin -o build/solc contracts/your-contract.sol`, or `forge build` (artifacts in `build/forge/`), then deploy using the ABI and bytecode.
3. Go: add the contract name to `CONTRACTS` in the `Makefile`, run `make contracts` (compiled into `client/artifacts/`), and load it with `client.Artifact("your-contract")`.
