# defichain：Lab 2 实验框架

《操作系统与分布式系统》实验二：亲手实现一条 PoW 区块链（共识、区块执行、交易池、同步），然后在你自己的链上搭建 DeFi 经济，让脚本机器人和 LLM 驱动的 AI Agent 在里面交易，研究它们的行为。

- **实验指南：[docs/LAB2.md](docs/LAB2.md)**（从这里开始）
- 协议规范：[docs/SPEC.md](docs/SPEC.md) · DeFi 入门：[docs/DEFI.md](docs/DEFI.md) · Agent 开发：[docs/AGENTS.md](docs/AGENTS.md) · 工具：[docs/TOOLS.md](docs/TOOLS.md)

## 快速开始

需要 Go 1.26+（更老的版本会自动下载工具链；国内网络先 `go env -w GOPROXY=https://goproxy.cn,direct`）。

```bash
go build ./...                    # 编译
go test ./chain -run TestFramework   # 框架自检，应该全部通过
go run ./cmd/devnet               # 3 个节点的本地网络和交互控制台（输入 help）
go test ./chain ./tests           # Part 1 的公开测试（实现之前会失败）
go run ./cmd/labcheck             # 提交前自检
go run ./cmd/sim                  # Part 2：DeFi 仿真（需要先完成 Part 1）
```

## 组成

| 目录 | 内容 |
|---|---|
| `chain/` | 节点：PoW、区块头验证、区块执行与验证、交易池、分叉选择与重组、矿工、同步（Part 1 实现的 7 个 ★ 文件），以及框架：EVM（单笔交易）、P2P 与故障注入、JSON-RPC、管理接口 |
| `spec/` | 链参数、创世块、节点配置的格式 |
| `client/` | Go SDK：JSON-RPC 客户端、签名与 nonce 管理、合约 ABI、事件解码 |
| `devnet/`、`cmd/devnet` | 多进程本地网络：启动、分区、崩溃与重启、等待收敛 |
| `cmd/sim`、`agent/` | DeFi 仿真器与内置 Agent（套利、清算、噪声交易、借款人、LLM 交易者） |
| `contracts/` | Solidity：ERC-20、恒定乘积 AMM、借贷、两种预言机；Foundry 测试 |
| `pysdk/` | Python SDK 与示例 Agent（规则、LLM）、实验与画图脚本 |
| `tests/` | 多节点测试 |

这是一个教学系统，不是生产区块链：开发账户的私钥是公开的，节点不落盘，管理接口没有认证（只监听本机地址）。
