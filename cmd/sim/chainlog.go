package main

// The on-chain record of a run: every Swap, liquidity change, lending action, liquidation
// and oracle update of the market's contracts, read from the chain's logs at the end (so it
// covers every agent, built-in or external), written to chain_events.csv.

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"defichain/agent"
	"defichain/client"
)

// chainEvent is one row of chain_events.csv. Amounts are signed from the actor's point of
// view: weth/usd > 0 means the actor received that much (a swap selling WETH has weth < 0,
// usd > 0; a deposit has weth < 0; a borrow has usd > 0).
type chainEvent struct {
	T           float64 // seconds since the run started (block timestamp; negative = setup)
	Block       uint64
	Tx          common.Hash
	Kind        string // swap, add_liquidity, remove_liquidity, deposit, withdraw, borrow, repay, liquidate, oracle
	Actor       string // agent name, or a short address
	User        string // liquidations: the liquidated borrower
	WETH, USD   float64
	Price       float64 // swap: average price; oracle: the new price
	ReserveWETH float64 // pool reserves after a swap
	ReserveUSD  float64
	user        common.Address // liquidations: the borrower's address
}

var chainHeader = []string{"t", "block", "tx", "kind", "actor", "user", "weth", "usd", "price", "reserve_weth", "reserve_usd"}

func (e chainEvent) row() []string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', 6, 64) }
	return []string{strconv.FormatFloat(e.T, 'f', 2, 64), fmt.Sprint(e.Block), e.Tx.Hex(), e.Kind, e.Actor, e.User,
		f(e.WETH), f(e.USD), f(e.Price), f(e.ReserveWETH), f(e.ReserveUSD)}
}

// name is the agent name of an address, or its short form.
func (w *world) name(a common.Address) string {
	if n, ok := w.names[a]; ok {
		return n
	}
	if a == w.deployer.Account {
		return "deployer"
	}
	return a.Hex()[:10]
}

// chainEvents reads the market contracts' logs from block 1 to the head.
func (w *world) chainEvents(ctx context.Context) ([]chainEvent, error) {
	rpc := w.deployer.RPC
	head, err := rpc.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	m := w.market
	addrs := []common.Address{m.Pool, m.Lending, m.Feed}
	var logs []*types.Log
	for from := uint64(1); from <= head; from += 5000 {
		to := min(from+4999, head)
		part, err := rpc.GetLogs(ctx, client.FilterQuery{FromBlock: hexutil.EncodeUint64(from), ToBlock: hexutil.EncodeUint64(to), Addresses: addrs})
		if err != nil {
			return nil, fmt.Errorf("eth_getLogs %d-%d: %w", from, to, err)
		}
		logs = append(logs, part...)
	}
	times := map[uint64]float64{}
	blockTime := func(n uint64) float64 {
		if t, ok := times[n]; ok {
			return t
		}
		t := 0.0
		if b, err := rpc.BlockByNumber(ctx, int64(n)); err == nil && b != nil {
			t = float64(b.Time) - float64(w.t0.Unix())
		}
		times[n] = t
		return t
	}
	var out []chainEvent
	for _, l := range logs {
		if e, ok := w.decode(l); ok {
			e.Block, e.Tx, e.T = l.BlockNumber, l.TxHash, blockTime(l.BlockNumber)
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Block < out[j].Block })
	return out, nil
}

var (
	ammEvents     = []string{"Swap", "AddLiquidity", "RemoveLiquidity"}
	lendingEvents = []string{"Deposit", "Withdraw", "Borrow", "Repay", "Liquidate"}
)

// decode turns one market log into a chainEvent.
func (w *world) decode(l *types.Log) (chainEvent, bool) {
	m := w.market
	f := func(v any) float64 {
		if x, ok := v.(*big.Int); ok {
			return agent.F(x)
		}
		return 0
	}
	addr := func(v any) common.Address { a, _ := v.(common.Address); return a }
	var con client.Contract
	var names []string
	switch l.Address {
	case m.Pool:
		con, names = client.AMMContract, ammEvents
	case m.Lending:
		con, names = client.LendingContract, lendingEvents
	case m.Feed:
		con, names = client.FeedOracleContract, []string{"PriceUpdated"}
	default:
		return chainEvent{}, false
	}
	for _, name := range names {
		ev, err := con.DecodeEvent(name, l)
		if err != nil {
			continue
		}
		e := chainEvent{}
		switch name {
		case "Swap":
			e.Kind, e.Actor = "swap", w.name(addr(ev["trader"]))
			in, out := f(ev["amountIn"]), f(ev["amountOut"])
			if addr(ev["tokenIn"]) == m.WETH {
				e.WETH, e.USD = -in, out
				if in > 0 {
					e.Price = out / in
				}
			} else {
				e.USD, e.WETH = -in, out
				if out > 0 {
					e.Price = in / out
				}
			}
			e.ReserveWETH, e.ReserveUSD = f(ev["reserve0"]), f(ev["reserve1"])
		case "AddLiquidity", "RemoveLiquidity":
			sign := -1.0
			e.Kind = "add_liquidity"
			if name == "RemoveLiquidity" {
				sign, e.Kind = 1, "remove_liquidity"
			}
			e.Actor, e.WETH, e.USD = w.name(addr(ev["provider"])), sign*f(ev["amount0"]), sign*f(ev["amount1"])
		case "Deposit":
			e.Kind, e.Actor, e.WETH = "deposit", w.name(addr(ev["user"])), -f(ev["amount"])
		case "Withdraw":
			e.Kind, e.Actor, e.WETH = "withdraw", w.name(addr(ev["user"])), f(ev["amount"])
		case "Borrow":
			e.Kind, e.Actor, e.USD = "borrow", w.name(addr(ev["user"])), f(ev["amount"])
		case "Repay":
			e.Kind, e.Actor, e.USD = "repay", w.name(addr(ev["user"])), -f(ev["amount"])
		case "Liquidate":
			e.Kind, e.Actor, e.User, e.user = "liquidate", w.name(addr(ev["liquidator"])), w.name(addr(ev["user"])), addr(ev["user"])
			e.USD, e.WETH = -f(ev["repaid"]), f(ev["seized"])
			if e.WETH > 0 {
				e.Price = -e.USD / e.WETH
			}
		case "PriceUpdated":
			e.Kind, e.Actor, e.Price = "oracle", "oracle", f(ev["price"])
		}
		return e, true
	}
	return chainEvent{}, false
}

func writeChainEvents(out io.Writer, events []chainEvent) error {
	cw := csv.NewWriter(out)
	cw.Write(chainHeader) //nolint:errcheck
	for _, e := range events {
		cw.Write(e.row()) //nolint:errcheck
	}
	cw.Flush()
	return cw.Error()
}

// liqStats are the liquidation statistics of a run, from the on-chain Liquidate events
// (whoever the liquidator was), valued at the exchange ("truth") price of the moment.
type liqStats struct {
	count, wrongful   int
	repaid, penalty   float64
	byUser            map[string]float64 // penalty paid per liquidated agent
	liquidatedByAgent map[string]int
}

func (w *world) liquidationStats(ctx context.Context, events []chainEvent, samples []agent.Sample) liqStats {
	st := liqStats{byUser: map[string]float64{}, liquidatedByAgent: map[string]int{}}
	truthAt := func(t float64) float64 {
		p := w.truth.Price()
		for i, s := range samples {
			if i == 0 || s.T <= t {
				p = s.Truth
			}
			if s.T > t {
				break
			}
		}
		return p
	}
	view := func(method string, who common.Address, block uint64) float64 {
		data, err := client.LendingContract.Calldata(method, who)
		if err != nil {
			return 0
		}
		ret, err := w.deployer.RPC.EthCallAt(ctx, client.CallArgs{From: w.deployer.Account, To: &w.market.Lending, Data: data}, block)
		if err != nil {
			return 0
		}
		out, err := client.LendingContract.Decode(method, ret)
		if err != nil || len(out) == 0 {
			return 0
		}
		x, _ := out[0].(*big.Int)
		return agent.F(x)
	}
	for _, e := range events {
		if e.Kind != "liquidate" {
			continue
		}
		truth := truthAt(e.T)
		repaid, seized := -e.USD, e.WETH
		pen := seized*truth - repaid
		st.count++
		st.repaid += repaid
		st.penalty += pen
		st.byUser[e.User] += pen
		st.liquidatedByAgent[e.User]++
		// Wrongful: the position was healthy at the real price, just before the block.
		if e.Block > 0 {
			debt, coll := view("debtOf", e.user, e.Block-1), view("collateralOf", e.user, e.Block-1)
			if debt > 0 && coll*truth*0.8/debt >= 1 {
				st.wrongful++
			}
		}
	}
	return st
}
