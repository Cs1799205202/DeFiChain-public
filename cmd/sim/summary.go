package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/agent"
	"defichain/devnet"
)

// summary is the machine-readable result of one run (summary.json).
type summary struct {
	Config config `json:"config"`

	TruthEnd  float64 `json:"truthEnd"`
	OracleEnd float64 `json:"oracleEnd"`
	PoolEnd   float64 `json:"poolEnd"`
	// Protocol solvency, valued at the truth price.
	BadDebtEnd   float64 `json:"badDebtEnd"`   // sum of max(0, debt - collateral*truth) at the end
	BadDebtMax   float64 `json:"badDebtMax"`   // worst sample during the run
	InsolventEnd int     `json:"insolventEnd"` // positions with debt > collateral value
	DebtEnd      float64 `json:"debtEnd"`

	ChainEvents       int     `json:"chainEvents"` // rows in chain_events.csv
	Liquidations      int     `json:"liquidations"`
	WrongfulLiq       int     `json:"wrongfulLiquidations"` // liquidated while healthy at the truth price
	LiqRepaid         float64 `json:"liqRepaid"`
	LiqPenalty        float64 `json:"liqPenalty"` // value transferred from borrowers to liquidators, at truth
	LiqSkipped        int     `json:"liqSkippedUnprofitable"`
	MaxOracleLag      float64 `json:"maxOracleLagPct"` // max |oracle-truth|/truth over samples
	MaxBlockGap       float64 `json:"maxBlockGap"`     // longest time (s) without a new canonical block
	Height            uint64  `json:"height"`
	ConsistentReplica bool    `json:"consistentReplicas"` // all nodes on the same head at the end
	Reorgs            int     `json:"reorgs"`             // the most reorgs any node saw
	MaxReorgDepth     int     `json:"maxReorgDepth"`      // the deepest (blocks removed)

	// Cascade: how far the exchange price fell below its own start, against how far the
	// scripted fundamental fell; the difference is what trading on the chain added.
	TruthMin       float64 `json:"truthMin"`
	FundamentalMin float64 `json:"fundamentalMin"`
	MaxImpactPct   float64 `json:"maxImpactPct"` // largest downward trade impact, percent

	Agents   []agentResult     `json:"agents"`
	Personas map[string]string `json:"personas"` // LLM agent name -> persona
	Faults   []agent.Event     `json:"faults,omitempty"`
	Accounts map[string]string `json:"accounts"` // agent name -> address, for positions.csv
}

type agentResult struct {
	Name       string  `json:"name"`
	Start      float64 `json:"start"` // net worth at truth when the clock started
	End        float64 `json:"end"`   // net worth at truth at the end
	PnL        float64 `json:"pnl"`
	HoldPnL    float64 `json:"holdPnl"` // PnL of doing nothing with the starting portfolio
	Alpha      float64 `json:"alpha"`   // PnL - HoldPnL
	Liquidated int     `json:"liquidated"`
	Penalty    float64 `json:"penalty"` // liquidation penalty paid
	Health     float64 `json:"health"`
	Decisions  int     `json:"decisions"`
	Trades     int     `json:"trades"`
	Holds      int     `json:"holds"`
	Errors     int     `json:"errors"`
	Reverts    int     `json:"reverts"`
	Position   string  `json:"position"`
}

func (w *world) summarize(ctx context.Context, events []agent.Event, samples []agent.Sample) summary {
	s := summary{Config: w.c, Accounts: map[string]string{}, Personas: w.personas}
	for addr, name := range w.names {
		s.Accounts[name] = addr.Hex()
	}
	// Read the final state on the recovered cluster; retry briefly while it settles.
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	truth := w.truth.Price()
	snap, err := w.market.Read(rctx, w.deployer)
	for err != nil && rctx.Err() == nil {
		time.Sleep(500 * time.Millisecond)
		snap, err = w.market.Read(rctx, w.deployer)
	}
	s.TruthEnd = truth
	if snap != nil {
		s.OracleEnd, s.PoolEnd, s.Height = snap.FeedPrice, snap.PoolPrice, snap.Block
	}
	if positions, err := agent.Positions(rctx, w.deployer, w.market); err == nil {
		for _, p := range positions {
			s.DebtEnd += p.Debt
			if short := p.Debt - p.Collateral*truth; short > 0 {
				s.BadDebtEnd += short
				s.InsolventEnd++
			}
		}
	}
	var lastHeight uint64
	var lastChange float64
	for i, smp := range samples {
		if s.TruthMin == 0 || smp.Truth < s.TruthMin {
			s.TruthMin = smp.Truth
		}
		if s.FundamentalMin == 0 || (smp.Fundamental > 0 && smp.Fundamental < s.FundamentalMin) {
			s.FundamentalMin = smp.Fundamental
		}
		s.MaxImpactPct = math.Max(s.MaxImpactPct, -math.Expm1(smp.Impact)*100)
		s.BadDebtMax = math.Max(s.BadDebtMax, smp.BadDebt)
		if smp.Oracle > 0 {
			s.MaxOracleLag = math.Max(s.MaxOracleLag, math.Abs(smp.Oracle-smp.Truth)/smp.Truth*100)
		}
		if i == 0 || smp.Height != lastHeight {
			if i > 0 {
				s.MaxBlockGap = math.Max(s.MaxBlockGap, smp.T-lastChange)
			}
			lastHeight, lastChange = smp.Height, smp.T
		}
	}

	// Liquidations come from the chain's logs, so they count every liquidator, external
	// agents included; chain_events.csv is the full on-chain record of the run.
	chainEvs, err := w.chainEvents(rctx)
	if err != nil {
		w.log.Printf("reading the chain's logs: %v", err)
	}
	s.ChainEvents = len(chainEvs)
	onChain := map[string]int{} // executed market transactions per agent
	for _, e := range chainEvs {
		if e.Kind != "oracle" {
			onChain[e.Actor]++
		}
	}
	if w.c.Out != "" {
		if f, err := os.Create(filepath.Join(w.c.Out, "chain_events.csv")); err == nil {
			writeChainEvents(f, chainEvs) //nolint:errcheck
			f.Close()
		}
	}
	ls := w.liquidationStats(rctx, chainEvs, samples)
	s.Liquidations, s.WrongfulLiq, s.LiqRepaid, s.LiqPenalty = ls.count, ls.wrongful, ls.repaid, ls.penalty
	penalty, liquidated := ls.byUser, ls.liquidatedByAgent
	byAgent := map[string]*agentResult{}
	for _, e := range events {
		switch {
		case e.Kind == "liq-skip":
			s.LiqSkipped++
		case e.Agent == "sim":
			s.Faults = append(s.Faults, e)
		}
		if strings.HasPrefix(e.Kind, "llm-") {
			r := byAgent[e.Agent]
			if r == nil {
				r = &agentResult{}
				byAgent[e.Agent] = r
			}
			switch e.Kind {
			case "llm-error":
				r.Errors++
			case "llm-decision":
				r.Decisions++
			case "llm-hold":
				r.Holds++
			case "llm-reverted":
				r.Reverts++
			case "llm-invalid", "llm-was-liquidated":
			default:
				r.Trades++ // an executed action
			}
		}
	}

	// Starting portfolios valued at the end price give the buy-and-hold benchmark.
	type acct struct {
		name string
		addr common.Address
	}
	var accounts []acct
	for _, b := range w.bases {
		accounts = append(accounts, acct{b.ID, b.C.Account})
	}
	for _, e := range w.external {
		accounts = append(accounts, acct{e.name, e.addr})
		d, _, h, errs := e.decisions()
		// Trades: what actually executed on chain (planned actions may have reverted).
		byAgent[e.name] = &agentResult{Decisions: d, Trades: onChain[e.name], Holds: h, Errors: errs}
		w.personas[e.name] = "external"
	}
	for _, b := range accounts {
		if _, ok := w.start[b.name]; !ok {
			continue
		}
		h, err := w.market.Holdings(rctx, w.deployer, b.addr)
		if err != nil {
			continue
		}
		r := agentResult{Name: b.name, Start: w.start[b.name], End: h.NetWorth(truth) + w.offChain(b.name, truth), Liquidated: liquidated[b.name],
			Penalty: penalty[b.name], Health: h.Health, Position: h.String()}
		if st, ok := w.startHold[b.name]; ok {
			r.HoldPnL = st.NetWorth(truth) - r.Start
		}
		r.PnL = r.End - r.Start
		r.Alpha = r.PnL - r.HoldPnL
		if math.IsInf(r.Health, 1) {
			r.Health = -1 // JSON cannot encode +Inf; -1 means no debt
		}
		if x := byAgent[b.name]; x != nil {
			r.Decisions, r.Trades, r.Holds, r.Errors, r.Reverts = x.Decisions, x.Trades, x.Holds, x.Errors, x.Reverts
		}
		s.Agents = append(s.Agents, r)
	}
	s.ConsistentReplica = w.replicasAgree()
	s.Reorgs, s.MaxReorgDepth = w.reorgCount()
	return s
}

// offChain is the value an agent holds outside the chain (its exchange account).
func (w *world) offChain(name string, price float64) float64 {
	for _, a := range w.agents {
		if v, ok := a.(interface{ OffChainValue(float64) float64 }); ok && a.Name() == name {
			return v.OffChainValue(price)
		}
	}
	return w.exchange.value(name, price)
}

// replicasAgree reports whether every running node ends on the same head (and so the same
// state). Miners stop at the end of the run only when the process exits, so allow a short
// time for the last block to propagate.
//
// Two branches of equal total difficulty are a tie that no node breaks by itself (each keeps
// the block it saw first) until another block is mined; so if the nodes still disagree once
// the messages in flight have arrived, mine one more block and check again.
func (w *world) replicasAgree() bool {
	if err := w.net.SetMiningAll(false); err != nil {
		return false
	}
	wait := func(d time.Duration) bool {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		_, err := w.net.WaitConverged(ctx)
		return err == nil
	}
	if wait(5 * time.Second) {
		return true
	}
	if heads, err := w.net.Heads(); err == nil {
		w.log.Printf("nodes disagree at the end (%s): mining a tie-breaking block", devnet.FormatHeads(heads))
	}
	if alive := w.net.Alive(); len(alive) > 0 {
		alive[0].Mine(1) //nolint:errcheck // checked by the convergence below
	}
	return wait(15 * time.Second)
}

func (w *world) print(s summary) {
	fmt.Println()
	fmt.Println("==================== report ====================")
	fmt.Printf("height %d, nodes agree: %v, longest wait for a block %.0fs, reorgs %d (deepest %d blocks)\n", s.Height, s.ConsistentReplica,
		s.MaxBlockGap, s.Reorgs, s.MaxReorgDepth)
	fmt.Printf("truth %.2f  oracle %.2f  pool %.2f  (max oracle lag %.1f%%)\n", s.TruthEnd, s.OracleEnd, s.PoolEnd, s.MaxOracleLag)
	fmt.Printf("lending: debt %.0f, bad debt %.0f (max %.0f), insolvent positions %d\n", s.DebtEnd, s.BadDebtEnd, s.BadDebtMax, s.InsolventEnd)
	fmt.Printf("liquidations %d (wrongful %d, skipped unprofitable %d), repaid %.0f, penalty %.0f\n",
		s.Liquidations, s.WrongfulLiq, s.LiqSkipped, s.LiqRepaid, s.LiqPenalty)
	if start := w.c.Price; start > 0 {
		fmt.Printf("drawdown: exchange %.1f%%, fundamental %.1f%% (max trade impact %.1f%%)\n",
			(1-s.TruthMin/start)*100, (1-s.FundamentalMin/start)*100, s.MaxImpactPct)
	}
	fmt.Printf("\n%-16s %10s %10s %9s %9s %9s %4s %8s  %s\n", "agent", "start", "end", "pnl", "hold-pnl", "alpha", "liq", "penalty", "llm d/t/h/e/r")
	for _, a := range s.Agents {
		llm := ""
		if a.Decisions+a.Errors > 0 {
			llm = fmt.Sprintf("%d/%d/%d/%d/%d", a.Decisions, a.Trades, a.Holds, a.Errors, a.Reverts)
		}
		name := a.Name
		if p := s.Personas[a.Name]; p != "" {
			name += "/" + p
		}
		fmt.Printf("%-16s %10.0f %10.0f %+9.0f %+9.0f %+9.0f %4d %8.0f  %s\n", name, a.Start, a.End, a.PnL, a.HoldPnL, a.Alpha, a.Liquidated, a.Penalty, llm)
	}
}
