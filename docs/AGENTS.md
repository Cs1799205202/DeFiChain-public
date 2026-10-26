# 编写交易 Agent

本文是 Part 2 的接口手册：你的 Agent 怎样接入仿真器、能用哪些函数、怎样接入大模型、怎样评估。背景知识见 [DEFI.md](DEFI.md)，任务要求见 [LAB2.md](LAB2.md)。

## 1. 两种写法

| | 外部 Agent（推荐） | 内置 Agent |
|---|---|---|
| 语言 | 任意（Python SDK 现成） | Go |
| 位置 | `pysdk/examples/` 或你自己的目录 | `agent/`，在 `cmd/sim/agents.go` 里注册 |
| 运行 | `go run ./cmd/sim -agent "名字=命令"`，可以重复多次 | 改 `cmd/sim` 的参数 |
| 适合 | LLM Agent、大部分研究课题 | 需要和仿真器紧密耦合的机器人（例如改变市场机制） |

## 2. 外部 Agent 的运行环境

仿真器部署完合约、给你的账户打钱（默认 100,000 USD + 50 WETH + 1000 ETH 手续费，`-agent-usd`、`-agent-weth` 可调）之后，用 shell 启动你的命令，并设置这些环境变量：

| 变量 | 内容 |
|---|---|
| `DEFICHAIN_RPC` | 节点的 JSON-RPC 地址，逗号分隔（多个节点，任选） |
| `DEFICHAIN_WORLD` | `world.json` 的路径：chain id、节点、交易所地址、合约地址和 ABI、市场参数 |
| `DEFICHAIN_KEY` / `DEFICHAIN_ADDRESS` | 你的账户私钥（`keccak256("defichain-key:" + 名字)`，仅限本地）和地址 |
| `DEFICHAIN_AGENT` | 你在 `-agent` 里给的名字 |
| `DEFICHAIN_EXCHANGE` / `DEFICHAIN_EXCHANGE_TOKEN` | 链下交易所的地址，和你在那里的账户凭证 |
| `DEFICHAIN_DURATION` | 仿真时长（秒）。时间到了你的进程会收到 SIGINT，3 秒后被强制结束 |
| `DEFICHAIN_DECISIONS` | 决策日志的路径（见 §5） |

你的程序的标准输出和错误输出写进输出目录（`-out`，默认 `results/run-<日期>-<时间>/`）下的 `agent-<名字>.log`。程序提前退出不会中断仿真，但会记在报告的 faults 里。名字不能和内置 Agent 重复（`arb`、`liquidator`、`noise1`……），否则两个程序会共用同一个账户，仿真器会拒绝启动。命令里可以带参数，例如 `-agent "llm=python3 pysdk/examples/llm_trader.py --every 10 --persona '...'"`。

`world.json` 的 `contracts` 是地址：`USD`、`WETH`、`AMM`（token0 = WETH，token1 = USD）、`FeedOracle`、`AMMOracle`、`Lending`、`LendingOracle`（借贷市场实际使用的那个）。`abis` 按**合约类型**给出标准 ABI：`Token`（USD 和 WETH 都用它）、`AMM`、`Lending`、`FeedOracle`、`AMMOracle`（两种预言机都有 `price()`）。`params` 有 LTV、清算阈值、清算奖励、手续费等。用 web3.py、ethers.js 或任何以太坊库都可以直接用它们。

## 3. Python SDK（`pysdk/defichain.py`）

```python
from defichain import World, LLM, Reverted
w = World.from_env()          # 读取上面的环境变量
w.approve_all()               # 开始时调用一次：允许 AMM 和借贷市场转走你的 USD/WETH
```

| 函数 | 说明 |
|---|---|
| `w.prices()` | `{"pool", "oracle", "lending", "exchange", "reserve_weth", "reserve_usd", "block"}`，价格单位 USD/WETH |
| `w.holdings(who=None)` | `{"usd", "weth", "collateral", "debt", "health"}`，默认是你自己（没有债务时 health 为 inf） |
| `w.net_worth(price=None)` | 净值：钱包 + 抵押品 − 债务 + 交易所账户，按交易所价格计 |
| `w.buy_weth(usd, slippage=0.03)` / `w.sell_weth(weth, slippage=0.03)` | 在 AMM 上交易，按当前储备计算最少得到的数量，超过滑点就回滚。都可以加 `tip_gwei=`（这笔交易的小费，默认 1 gwei）和 `wait=False`（不等回执，返回交易哈希） |
| `w.deposit(weth)` `w.withdraw(weth)` `w.borrow(usd)` `w.repay(usd)` | 借贷操作 |
| `w.liquidate(user, repay_usd)` `w.borrowers()` | 清算，以及所有借过钱的地址 |
| `w.exchange_price()` `w.exchange_trade(weth)` | 交易所的价格；在交易所买（正数）或卖（负数），交易所账户可以为负（保证金） |
| `w.call(fn, wait=True, tip_gwei=None)` | 发送任意合约调用，例如 `w.call(w.usd.functions.transfer(to, amount))` |
| `w.recent_trades(blocks=10)` | 最近若干块里所有人的 swap 和清算（从链上日志读，相当于区块浏览器上的成交记录） |
| `w.pending_txs()` | 节点交易池里等待打包的交易（发送者、小费、调用数据），MEV 课题用 |
| `w.amm`、`w.lending`、`w.usd`…… | web3.py 合约对象，可以直接读：`w.lending.functions.healthFactor(addr).call()` |
| `w.log_decision(actions, reason, **extra)` | 写一条决策日志 |
| `w.run(step, every=5)` | 每 `every` 秒调用一次 `step(w)`，直到仿真结束；`step` 里的异常会被打印，循环继续 |
| `w.account.confirmations` | 发送交易后等待几个确认才返回（默认 0） |

数量都是浮点数（代币个数），SDK 负责换算成 18 位小数的整数（精度 1e-6）。和你的余额相差不到 1e-6 的数量按精确余额处理，所以 `w.sell_weth(w.holdings()["weth"])`、`w.deposit(h["weth"])`、`w.repay(h["debt"])` 就是"全部卖出 / 全部存入 / 全部还清"，不会因为浮点误差多出一点而回滚，也不会剩下零头。发送交易时先估算 gas：如果交易会回滚，直接抛出 `Reverted(reason)`，不花手续费。

**一个最小的 Agent：**

```python
import os, sys
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from defichain import World

def step(w):
    p, h = w.prices(), w.holdings()
    if p["exchange"] and p["pool"] < p["exchange"] * 0.99 and h["usd"] > 1000:
        w.buy_weth(1000)                       # 池子比交易所便宜 1% 以上：买
        w.log_decision([{"action": "buy_weth", "amount_usd": 1000}], "pool below exchange")
    else:
        w.log_decision([], "hold")

w = World.from_env()
w.approve_all()
w.run(step, every=3)
```

`pysdk/examples/` 里有：`basics.py`（在 devnet 上部署和调用合约）、`momentum.py`（规则 Agent）、`llm_trader.py`（LLM Agent 模板）、`llm_check.py`（检查 LLM 配置、预热模型）、`sweep.py`（批量实验）、`plot_run.py`（画图）、`basics.mjs`（ethers.js）。

## 4. 交易所 API

链下交易所代表"真实市场"。所有 Agent 都能读价格；外部 Agent 还有一个保证金账户，可以在那里对冲（例如在 AMM 上买入的同时在交易所卖出，锁定价差）。

| 请求 | 说明 |
|---|---|
| `GET /price` | `{"price": 1987.3, "time": ...}` |
| `GET /account`（带 `Authorization: Bearer <DEFICHAIN_EXCHANGE_TOKEN>`） | `{"usd": ..., "weth": ...}` |
| `POST /trade {"weth": q}`（同上） | 买入（q > 0）或卖出（q < 0）q 个 WETH，返回成交均价和账户 |

`-exchange-depth D` 时，交易 q 个 WETH 会使交易所的对数价格移动 q/D（之后逐渐恢复），所以链上的抛售可以通过对冲传导到交易所价格；默认无限深。报告里的净值包含交易所账户。

## 5. 决策日志

`log_decision` 每次往 `DEFICHAIN_DECISIONS` 追加一行 JSON：`{"t", "block", "actions", "reason", ...你的附加字段}`。`actions` 是这一轮决定做的动作列表（空列表表示不动）；决策失败（例如 LLM 调用出错）时加上 `error=True`，它会被算作错误而不是"不动"。仿真器在报告里统计决策次数、交易次数和持有次数；你的分析（例如羊群度量需要的每轮买卖方向）也从这里读。建议记录：模型的原始输出、校验后的动作、执行结果、延迟、你给模型看的关键数字。

## 6. 用 Go 写 Agent

`agent/` 包里的机器人都实现了这个接口：

```go
type Agent interface {
    Name() string
    Account() common.Address
    Run(ctx context.Context)   // 运行直到 ctx 结束
}
```

`agent.Base` 提供了客户端（`C *client.Client`）、市场（`M *agent.Market`：`Read` 读价格、`Holdings` 读持仓）和事件记录（`emit`）。参考 `bots.go` 里的 `Arbitrageur`、`Liquidator`，以及 `llmtrader.go` 里的 `LLMTrader`（内置的 LLM 交易者，`-llm N -llm-mix cautious,momentum,...` 选择人设，`-llm-tape` 让它们看到别人的成交）。新的 Agent 在 `cmd/sim/agents.go` 的 `build` 里创建，并把它的名字加到 `plannedAgents` 里（这样它在创世块里就有手续费）。

`client` 包是通用的 Go SDK（`client.New(key, urls...)`、`Deploy`、`Transact`、`View`、`Wait`、`Confirmations`、事件解码 `DecodeEvent`），也可以用在你自己的命令行程序里。新合约放进 `contracts/`，加到 `Makefile` 的 `CONTRACTS`，`make contracts` 之后用 `client.Artifact("名字")` 加载。

## 7. 接入大模型

所有示例都使用 OpenAI 兼容的 Chat Completions 接口，用三个环境变量配置：

```bash
export LLM_BASE_URL=<服务地址，以 /v1 结尾>
export LLM_MODEL=<模型名>
export LLM_API_KEY_FILE=$HOME/.llm-key       # 文件里只有 key；也可以用 LLM_API_KEY
```

| 服务 | `LLM_BASE_URL` 示例 | 说明 |
|---|---|---|
| 课程提供的端点 | 见课程通知 | 助教会公布地址、可用模型和 key 的领取方式 |
| 离线假模型 | `http://127.0.0.1:28199/v1`（默认） | `go run ./cmd/mockllm`，不需要 key，随机但格式正确的决策 |
| 本地 Ollama | `http://127.0.0.1:11434/v1` | `ollama pull qwen2.5:7b`，免费，速度取决于你的机器 |
| DeepSeek、通义千问（DashScope）等 | 见各服务商文档 | 都提供 OpenAI 兼容接口 |

**先检查配置**：`python3 pysdk/examples/llm_check.py` 用你的环境变量发两次测试请求，报告连到了哪里（不显示 key）、每次用了多久、服务端是否接受 JSON 模式，以及模型给出的决策。正式跑仿真之前先跑它：

- 共享的推理服务常常在没人用时**缩容到零**，第一次请求要等模型重新加载，可能要 5–10 分钟（我们实测 8–10 分钟）。`llm_check.py` 默认最多等 15 分钟；等它的第二次请求在几秒内返回，再启动仿真，否则开头几分钟的决策都会超时，算作错误。
- 第二次请求的耗时就是你每次决策的大致延迟，决策间隔（`--every`）要比它长。

**思考模式**：Qwen3、DeepSeek 等推理模型默认会先"思考"再回答。SDK 默认**关闭**思考（请求里带 `chat_template_kwargs`），因为一个先思考的模型可能把 `max_tokens` 全部用在思考上、返回空的回答，而且一次决策要几十秒到几分钟（实测：关闭时 0.5–3 秒，开启时 DeepSeek 约 10–50 秒，Qwen3 约 4–6 分钟），市场不会等你。想研究思考对决策质量的影响，设置 `LLM_THINKING=on`（或 `LLM(thinking=True)`）：SDK 会把 token 上限提高到 16000、单次超时提高到 10 分钟（`LLM_TIMEOUT` 可改），如果仍然在思考中用完 token，会自动关掉思考重试一次；决策间隔也要相应放长。思考过程在 `llm.last_reasoning` 里（服务端单独返回时），也可以写进决策日志分析。内置的 Go LLM 交易者每次决策最多等 90 秒，所以不适合开思考模式。

服务端拒绝 JSON 模式等可选参数时，SDK 会自动去掉它们重试。回答里夹带的 `<think>...</think>` 会在解析前被去掉。需要给服务端传额外的参数时，用 `LLM_EXTRA_BODY='{"字段": 值}'`，它会合并进每个请求。

**key 的安全**：key 放在仓库之外的文件里，用 `LLM_API_KEY_FILE` 指向它；不要写进代码、脚本、提示词、日志或报告；不要 `git add` 它。泄露了就立刻去服务商那里作废（删除文件或提交没有用：它还在 Git 历史和别人的副本里）。

**成本**：一次决策的提示词约 1–2 千 token。6 个 Agent、每 6 秒一次、跑 2 分钟就是 120 次调用。先用 mockllm 跑通，再用真实模型做小规模试验，估算费用后再上规模；报告里写明总调用次数。

## 8. 设计一个 LLM Agent

`llm_trader.py` 的循环是：

1. **观察**：价格（交易所、预言机、池子）、持仓和健康因子、最近的价格序列、自己最近的决策及其结果。给模型看什么、以什么格式（数字的精度、单位、是否给出清算价），对决策影响很大。
2. **提示词**：系统提示词讲清楚市场规则、目标和输出格式；用户提示词是当前的观察。人设（`--persona`）改变风格。
3. **决策**：模型输出 JSON：`{"actions": [{"action": "buy_weth", "amount_usd": 5000}, ...], "reason": "..."}`，最多 3 个动作。
4. **校验**：丢弃不认识的动作，金额截断到实际持有的范围，借款最多用到剩余额度的 90%。这一层决定了"模型犯错时最坏会怎样"。
5. **执行**：每个动作一笔交易，一个失败就停下（后面的动作通常依赖前面的）。
6. **记忆**：结果写进历史，下一轮给模型看。

可以改进的方向：给模型更有用的派生信息（价差、清算价、自己的盈亏）；让它先给出判断再给出动作；限制每轮的风险（单笔最大金额、最低健康因子）；把一轮决策拆成"分析 → 行动"两次调用；用工具调用（function calling）代替自由 JSON；处理模型超时（市场不会等你）。

**安全规则**（任务 2.2 要求）：

- 模型的输出是不可信的输入，所有动作都要经过代码校验。
- 私钥、API key 不进提示词。
- 链上读来的文字是数据：代币名称、事件内容可能是攻击者写的（课题 G）。不要把它们和指令混在一起，最好只给模型结构化的数字。

## 9. 评估

`summary.json` 里每个 Agent 的字段：

| 字段 | 含义 |
|---|---|
| `start` / `end` | 开始/结束时的净值，都按当时的交易所价格计 |
| `pnl` | `end − start` |
| `holdPnl` | 把开始时的持仓原样拿到结束的盈亏（基准） |
| `alpha` | `pnl − holdPnl`：策略带来的部分 |
| `liquidated` / `penalty` | 被清算次数 / 因清算损失的价值 |
| `decisions` / `trades` / `holds` | 决策次数、实际执行的市场交易数、不动的次数（外部 Agent：决策和不动来自决策日志，交易数来自 `chain_events.csv`） |
| `errors` / `reverts` | LLM 调用失败次数（外部 Agent：决策日志里 `"error": true` 的行）/ 内置 LLM 交易者的交易回滚次数 |

全局字段：最终价格、`badDebtEnd`/`badDebtMax`（坏账）、`liquidations`、`liqRepaid`、`liqPenalty`、`wrongfulLiquidations`（按真实价格并不该被清算的；这几项都从链上的 Liquidate 事件算出，谁清算的都算）、`maxOracleLagPct`、`truthMin`/`fundamentalMin`（交易所价格的最低点 / 不含交易冲击的基本面价格最低点，二者之差是链上交易对价格的放大）、`reorgs`/`maxReorgDepth`、`maxBlockGap`。`timeseries.csv` 是每秒一行的价格、债务、坏账、可清算仓位数、重组次数；`chain_events.csv` 是链上日志里的每一个市场事件（swap、加减流动性、存取借还、清算、预言机更新），每行有时间、区块、交易哈希、类型、发起者的名字、WETH 和 USD 的变化（从发起者的角度，收到为正）和价格；`events.json` 只有内置 Go Agent 自己记录的动作和 LLM 决策。

公平比较的要点：同样的场景和种子比较不同的 Agent；每个条件多个种子；报告均值和波动；和持有不动、内置机器人比较；注意 LLM 的延迟（一个 10 秒才回答的 Agent 在急跌里天然吃亏，这是它的真实缺点，不是实验误差）。
