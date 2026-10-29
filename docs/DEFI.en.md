# DeFi Primer: Concepts Used in This Lab

This document uses the contracts from this lab (`contracts/`) to explain the DeFi knowledge needed for Part 2. All numbers can be verified on your devnet.

## 1. Accounts, Tokens, and Approvals

There are two kinds of accounts on-chain: **externally owned accounts** (controlled by private keys, can send transactions) and **contract accounts** (controlled by code, can only be called). ETH is the native currency of the chain, used to pay transaction fees (gas); other "coins" are just bookkeeping inside contracts.

**ERC-20** is the standard interface for token contracts: `balanceOf(address)`, `transfer(recipient, amount)`, `approve(spender, allowance)`, `transferFrom(payer, recipient, amount)`. Token amounts are integers with 18 decimals: 1 USD is `10^18` on-chain.

**Approve** is a fundamental DeFi action: to let an AMM take USD out of your account, you first `usd.approve(amm, allowance)`, and then the AMM calls `usd.transferFrom(you, amm, amount)` inside `swap`. So every Agent must approve first at the start (the SDK's `approve_all()`). An unlimited allowance (`2^256 − 1`) is convenient, but it also means the entire balance is exposed if the approved contract has a vulnerability — be cautious in the real world.

**Cost of a transaction**: `gasUsed × gasPrice`. This chain has a base fee of 0, so gasPrice is the tip you offer, all of which goes to miners. A transfer is about 21,000 gas, a swap about 70,000–100,000 gas. The higher the tip, the higher the priority miners give to including the transaction (this matters in §6 on MEV).

## 2. AMM: Automated Market Maker

Traditional exchanges use order books to match buyers and sellers. An **AMM** replaces that with a liquidity pool and a formula: the pool holds `x` WETH and `y` USD, and trades must keep `x · y = k` constant (Uniswap V2, this lab's `AMM.sol`).

**Price**: the pool's instantaneous price is `y / x` (USD/WETH). Initially 500 WETH / 1,000,000 USD, price 2000.

**How much USD you get for selling Δx WETH** (after the 0.3% fee):

```
Δx' = Δx × 0.997
Δy  = y × Δx' / (x + Δx')
```

Example: sell 10 WETH, `Δx' = 9.97`, `Δy = 1,000,000 × 9.97 / 509.97 ≈ 19,550 USD`, average price about 1955, roughly 2.3% below 2000: 0.3% of that is the fee, the rest is **price impact** (your own trade pushed the price down). The more you sell, the larger the impact: selling 50 WETH gives an average price of about 1813. After the sale the pool price becomes `(y − Δy)/(x + Δx)`, about 1922.

**Arbitrage keeps the price in line with the market**: when the pool price is below the external market, arbitrageurs buy in the pool and sell on the outside until the gap is smaller than the fee plus impact. So the AMM price always lags the true price slightly, and briefly deviates after a large trade.

**Slippage protection**: `swap(tokenIn, amountIn, minAmountOut)` reverts when the actual output is less than `minAmountOut` (`"slippage"`). Between the moment you see a quote and the moment your transaction is packed, someone else may have traded first: without slippage protection you can be "sandwiched" (§6).

**Liquidity providers (LPs)**: `addLiquidity` deposits both tokens at the current ratio and mints shares; `removeLiquidity` withdraws them. LPs earn fees, but price changes cause **impermanent loss**: when the price moves from p to p·r, the LP's asset value is `2√r/(1+r)` times what holding would have been (fees excluded); a doubling or halving of price costs about 5.7%.

## 3. Lending and Liquidation

`Lending.sol` is a simplified Aave/Compound: deposit WETH as collateral, borrow USD.

| Parameter | Value | Meaning |
|---|---|---|
| LTV | 75% | Can borrow at most 75% of collateral value |
| Liquidation threshold | 80% | Can be liquidated when debt exceeds 80% of collateral value |
| Liquidation bonus | 5% | Liquidator takes 5% extra collateral |
| Liquidation ratio | 50% | A liquidator can repay at most half the debt on the borrower's behalf in one liquidation |
| Interest rate | 1e-6 per second (simulation default) | Debt grows with block timestamp; over a simulation of minutes the interest is small |

**Health factor** `HF = collateral value × 80% / debt`; below 1 it can be liquidated. Collateral value is computed using the price from an **oracle**.

Example: deposit 10 WETH (price 2000, value 20,000), borrow 14,000 USD (LTV 70%), HF = 16,000/14,000 ≈ 1.14. When the price drops to 1750, HF = 1.0; when it drops to 1500, anyone can repay 7,000 USD and take `7,000 × 1.05 / 1500 = 4.9` WETH (worth 7,350): the borrower loses a 350 USD liquidation penalty.

**Why someone is willing to liquidate**: because it is profitable (5% bonus). After receiving WETH, the liquidator usually sells it immediately for USD, which causes the problem discussed in the next section.

**Bad debt**: when the price falls too fast for liquidations to catch up, the collateral value may drop below the debt; the shortfall is the protocol's bad debt (`badDebtEnd` and `badDebtMax` in `summary.json`, the `bad_debt` column in `timeseries.csv`).

## 4. Oracles

The lending contract does not know the value of WETH on its own; it has to ask an oracle. This lab has two:

- **FeedOracle**: a reporter writes the exchange price on-chain every second, like a simplified Chainlink. It is trustworthy but **lags**: the quote transaction must wait to be packed, so during a sharp market drop the on-chain price trails the real price (`maxOracleLagPct`), and liquidations are delayed accordingly.
- **AMMOracle**: reads the AMM pool's instantaneous price directly. It is real-time and needs no reporter, but it **can be manipulated**: a single large sell order can instantly push the pool price down, trigger a liquidation that should not have happened, and the attacker then buys back at the low price. Oracle manipulation of this kind is one of the largest loss-causing attack types in DeFi history. Defenses include time-weighted average prices (TWAP), multiple data sources, and price-change caps.

`-spot-oracle` makes the lending market use AMMOracle.

## 5. Liquidation Cascade

```
Price falls → some positions HF < 1 → liquidated → liquidator sells WETH into AMM → AMM price falls further
    ↑                                                                    │
    └──── (spot oracle: direct) / (feed oracle: propagated to exchange via arbitrageurs) ←─────┘
```

With the spot oracle this loop is direct. With the feed oracle, on-chain selling must first propagate to the exchange through arbitrageurs (they buy cheap in the pool and hedge-sell on the exchange) before the exchange price falls: `-exchange-depth` sets how deep the exchange is, and the shallower it is the stronger the propagation. The more borrowers' leverage is concentrated near similar liquidation price levels, the more violent the cascade.

## 6. MEV: The Value of Transaction Ordering

Miners decide which transactions go in a block and in what order (this lab's miners order by tip, high to low). Transactions waiting to be packed are visible to everyone in the transaction pool (`txpool_content`). Thus:

- **Front-running**: seeing a large buy order, race in first with a higher tip to buy, then sell after it pushes the price up.
- **Sandwich attack**: buy ahead of the victim's swap (high tip) and sell after it (low tip); the victim fills at a worse price and the attacker pockets the spread. The larger the victim's slippage tolerance, the more can be extracted.
- **Liquidation competition**: multiple liquidators compete for the same position; the higher the tip, the more likely it is to win.

This extractable value is called **MEV** (Maximal Extractable Value). Miners can do these things themselves, and may even reorganize the chain for MEV.

## 7. Finality and Confirmations

On a PoW chain, a transaction appearing in a block does not mean it is final: if another branch becomes heavier, this block gets revoked (the chain reorganization from Part 1). The more blocks stacked on top of a transaction (**confirmations**), the lower the probability of revocation. Section 11 of the Bitcoin whitepaper gives the probability that an attacker with hash-power share q catches up from k confirmations behind; it falls exponentially with k, but as long as q > 0 it is never zero, and when q ≥ 50% the attacker always succeeds (the "51% attack").

Impact on Agents:

- An Agent that treats seeing a receipt as "done" will misjudge its own position during a reorg.
- A log filter re-reports events from revoked blocks with `removed: true`; an Agent that processes events must be able to undo them.
- Waiting for confirmations is safer but slower: the market keeps moving while you wait.

## 8. Glossary

| Term | Meaning |
|---|---|
| gas / tip | Computation consumed by a transaction / price paid to miners per unit gas |
| nonce | Account's transaction sequence number; also the number a miner tries in PoW (the two are unrelated) |
| receipt | Result of a transaction's execution: success or revert, gas consumed, events emitted |
| event / log | Records written during contract execution, such as `Transfer`, `Swap`, `Liquidate` |
| revert | The contract refuses to execute (`require` fails); state is unchanged but gas is still paid |
| slippage | Difference between the actual fill price and the expected price |
| LTV / health factor | Ratio of debt to collateral value / how far from being liquidated |
| canonical chain / reorg / confirmations | See Part 1 and SPEC.en.md |

## 9. Further Reading

- Satoshi Nakamoto, *Bitcoin: A Peer-to-Peer Electronic Cash System*, 2008 (Sections 4, 5, 11)
- Ethereum Yellow Paper; [ethereum.org developer docs](https://ethereum.org/developers/docs/) (transactions, gas, EVM, smart contracts)
- Uniswap V2 Core whitepaper (Adams, Zinsmeister, Robinson, 2020)
- Qin et al., *An Empirical Study of DeFi Liquidations*, IMC 2021
- Daian et al., *Flash Boys 2.0: Frontrunning, Transaction Reordering, and Consensus Instability in Decentralized Exchanges*, IEEE S&P 2020
- Eyal & Sirer, *Majority is not Enough: Bitcoin Mining is Vulnerable*, FC 2014
- Lakonishok, Shleifer & Vishny, *The Impact of Institutional Trading on Stock Prices*, JFE 1992 (herding measure)
