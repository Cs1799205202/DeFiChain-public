package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/agent"
	"defichain/client"
	"defichain/devnet"
)

// world is one simulation: the devnet, the market and the agents.
type world struct {
	c        config
	net      *devnet.Net
	market   *agent.Market
	deployer *client.Client
	truth    *agent.Truth
	events   chan agent.Event
	log      *log.Logger

	agents []agent.Agent
	bases  []*agent.Base
	// start is each agent's net worth (at the truth price) when the clock starts, and
	// startHold the holdings themselves, for the buy-and-hold benchmark.
	start     map[string]float64
	startHold map[string]agent.Holdings
	// names maps account address to agent name, for reading liquidation events.
	names map[common.Address]string
	// personas maps LLM agent name -> persona key.
	personas map[string]string
	tape     *agent.Tape
	t0       time.Time
	notes    []agent.Event
	mu       sync.Mutex

	external []*external
	exchange *exchange
	nextNode int // agents are spread over the nodes, as users of a real network would be
}

type funded struct {
	a         agent.Agent
	base      *agent.Base
	usd, weth float64
	open      func(context.Context) error // opens a leveraged position during setup
}

// newClient is a client for the named account, preferring the next node in turn.
func (w *world) newClient(name string) *client.Client {
	c := client.NewFromName(name, w.net.URLs()...)
	c.RPC.Use(w.nextNode)
	w.nextNode++
	return c // -confirmations applies from t0 on (setT0), not to the setup
}

func (w *world) base(name string) *agent.Base {
	if !slices.Contains(plannedAgents(w.c), name) {
		panic("cmd/sim: agent " + name + " missing from plannedAgents (it would get no gas money)")
	}
	b := &agent.Base{ID: name, C: w.newClient(name), M: w.market, Log: w.log, Events: w.events}
	w.bases = append(w.bases, b)
	return b
}

// plannedAgents lists the accounts build() creates, so that they get ETH at genesis.
func plannedAgents(c config) []string {
	var out []string
	if !c.NoArb {
		out = append(out, "arb")
	}
	if c.Liquidators <= 1 {
		out = append(out, "liquidator")
	} else {
		for i := 0; i < c.Liquidators; i++ {
			out = append(out, fmt.Sprintf("liquidator%d", i+1))
		}
	}
	for i := 0; i < c.Noise; i++ {
		out = append(out, fmt.Sprintf("noise%d", i+1))
	}
	for i := 0; i < c.Borrowers; i++ {
		out = append(out, fmt.Sprintf("borrower%d", i+1))
	}
	for i := 0; i < c.LLM; i++ {
		out = append(out, fmt.Sprintf("llm%d", i+1))
	}
	if c.Managed {
		out = append(out, "mgr-passive", "mgr-rule", "mgr-llm")
	}
	return out
}

// build creates and funds every agent, and opens the leveraged positions.
func (w *world) build(ctx context.Context) error {
	c := w.c
	var fs []funded
	add := func(a agent.Agent, b *agent.Base, usd, weth float64, open func(context.Context) error) {
		fs = append(fs, funded{a, b, usd, weth, open})
	}
	// The reporter must be the deployer, the only account allowed to set the feed.
	rep := &agent.Base{ID: "oracle", C: w.deployer, M: w.market, Log: w.log, Events: w.events}
	w.bases = append(w.bases, rep)
	w.agents = append(w.agents, &agent.Reporter{Base: rep, Truth: w.truth, Every: c.OracleEvery})

	if !c.NoArb {
		b := w.base("arb")
		var ref *agent.Truth
		if c.ArbRef == "truth" {
			ref = w.truth
		}
		// It hedges on the exchange, so with a finite-depth exchange its trades move the
		// exchange price: the channel from on-chain selling to the oracle.
		add(&agent.Arbitrageur{Base: b, Truth: ref, MinEdge: 0.006, Every: 700 * time.Millisecond, Hedge: ref != nil}, b, 600_000, 300, nil)
	}
	for i := 0; i < max(1, c.Liquidators); i++ {
		name := "liquidator"
		var order *rand.Rand
		if c.Liquidators > 1 {
			name = fmt.Sprintf("liquidator%d", i+1)
			order = rand.New(rand.NewSource(c.Seed + int64(100+i)))
		}
		b := w.base(name)
		add(&agent.Liquidator{Base: b, Truth: w.truth, Every: 700 * time.Millisecond, Rand: order}, b, 600_000, 0, nil)
	}
	for i := 0; i < c.Noise; i++ {
		b := w.base(fmt.Sprintf("noise%d", i+1))
		add(&agent.NoiseTrader{Base: b, MaxUSD: 15_000, Every: 3 * time.Second, Rand: rand.New(rand.NewSource(c.Seed + int64(10+i)))}, b, 200_000, 100, nil)
	}
	for i := 0; i < c.Borrowers; i++ {
		ltv := c.LTVMin
		if c.Borrowers > 1 {
			ltv += (c.LTVMax - c.LTVMin) * float64(i) / float64(c.Borrowers-1)
		}
		b := w.base(fmt.Sprintf("borrower%d", i+1))
		br := &agent.Borrower{Base: b, Collateral: 50, TargetLTV: ltv}
		add(br, b, 0, 50, br.Open)
	}

	var llm *agent.LLM
	if c.LLM > 0 || c.Managed {
		var err error
		if llm, err = agent.NewLLMFromEnv(); err != nil {
			return err
		}
		w.log.Printf("LLM: %s (thinking %v)", llm.Model, llm.Thinking)
	}
	mix := strings.Split(c.LLMMix, ",")
	for i := 0; i < c.LLM; i++ {
		key := strings.TrimSpace(mix[i%len(mix)])
		text, ok := personas[key]
		if !ok {
			return fmt.Errorf("unknown persona %q (have %s)", key, strings.Join(personaOrder, ", "))
		}
		b := w.base(fmt.Sprintf("llm%d", i+1))
		lt := &agent.LLMTrader{Base: b, LLM: llm, Every: c.LLMEvery, Persona: key, Strategy: text, Truth: w.truth}
		if c.LLMTape {
			lt.Tape = w.tape
		}
		w.personas[b.ID] = key
		add(lt, b, 100_000, 50, nil)
	}
	if c.Managed {
		// Three identical looped longs: 50 WETH collateral, borrowed USD "spent" on WETH that
		// sits in the wallet, plus a small USD buffer. Only the management differs.
		sink := agent.Address("sink")
		walletWETH := 50 * c.ManagedLTV
		pos := func(b *agent.Base) agent.Borrower {
			return agent.Borrower{Base: b, Collateral: 50, TargetLTV: c.ManagedLTV, SpendTo: &sink}
		}
		bp := w.base("mgr-passive")
		passive := pos(bp)
		add(&passive, bp, 5_000, 50+walletWETH, passive.Open)

		br := w.base("mgr-rule")
		rule := &agent.Deleverager{Borrower: pos(br), Trigger: 1.25, Target: 1.6, Every: time.Second}
		add(rule, br, 5_000, 50+walletWETH, rule.Open)

		bl := w.base("mgr-llm")
		mp := pos(bl)
		lt := &agent.LLMTrader{Base: bl, LLM: llm, Every: c.LLMEvery, Truth: w.truth, Persona: "manager", Strategy: managerPersona}
		w.personas[bl.ID] = "manager"
		add(lt, bl, 5_000, 50+walletWETH, mp.Open)
	}

	// Fund everyone in one batch, then approvals and positions per account in parallel.
	var mints []agent.Call
	for _, f := range fs {
		mints = append(mints, w.market.FundCalls(f.base.C.Account, f.usd, f.weth)...)
		w.agents = append(w.agents, f.a)
	}
	for _, e := range w.external {
		mints = append(mints, w.market.FundCalls(e.addr, c.AgentUSD, c.AgentWETH)...)
	}
	if err := agent.Batch(ctx, w.deployer, mints); err != nil {
		return fmt.Errorf("fund: %w", err)
	}
	errs := make(chan error, len(fs))
	for _, f := range fs {
		go func() {
			if err := w.market.ApproveAll(ctx, f.base.C); err != nil {
				errs <- fmt.Errorf("%s approve: %w", f.base.ID, err)
				return
			}
			if f.open != nil {
				if err := f.open(ctx); err != nil {
					errs <- fmt.Errorf("%s open: %w", f.base.ID, err)
					return
				}
			}
			errs <- nil
		}()
	}
	for range fs {
		if err := <-errs; err != nil {
			return err
		}
	}
	// Starting net worth, after positions are open.
	w.start = map[string]float64{}
	w.startHold = map[string]agent.Holdings{}
	w.names = map[common.Address]string{}
	for _, f := range fs {
		h, err := w.market.Holdings(ctx, w.deployer, f.base.C.Account)
		if err != nil {
			return err
		}
		w.start[f.base.ID] = h.NetWorth(w.truth.Price())
		w.startHold[f.base.ID] = h
		w.names[f.base.C.Account] = f.base.ID
	}
	for _, e := range w.external {
		h, err := w.market.Holdings(ctx, w.deployer, e.addr)
		if err != nil {
			return err
		}
		w.start[e.name] = h.NetWorth(w.truth.Price())
		w.startHold[e.name] = h
		w.names[e.addr] = e.name
	}
	return nil
}

const managerPersona = `Persona: risk manager of a leveraged long. You hold WETH collateral in the lending market and WETH in
your wallet; the borrowed USD has already been spent. Your job is to keep the position alive through market moves without
needlessly destroying value: a liquidation costs a 5% penalty on the liquidated debt. You can sell wallet WETH for USD
(sell_weth) and repay debt (repay), deposit wallet WETH as extra collateral (deposit), or combine them in one plan.`

// personaOrder is the default mix, one of each.
var personaOrder = []string{"cautious", "momentum", "arbitrage", "contrarian", "degen", "panic"}

// personas are appended to the LLM system prompt. They describe distinct archetypes whose
// behaviour should differ visibly, so that any convergence between them is notable.
var personas = map[string]string{
	"cautious": `Persona: cautious value investor. Preserve capital first. Keep your health factor above 2 (borrow at most about
35% of your collateral's value). Prefer holding; trade only on large, clear opportunities.`,
	"momentum": `Persona: aggressive momentum trader. Follow the trend: buy WETH when the exchange price has been rising, sell when
it has been falling. Use leverage (deposit, borrow, buy) when the trend is strong. You accept liquidation risk for returns.`,
	"arbitrage": `Persona: arbitrageur. Trade only when the pool price deviates from the exchange price by more than the fee and
the price impact: buy WETH on the pool when it is cheaper than on the exchange, sell when it is dearer. Avoid directional
bets and leverage.`,
	"contrarian": `Persona: contrarian. Buy WETH after sharp drops and sell after sharp rises, betting on mean reversion. You may
borrow against collateral to buy dips, but keep your health factor above 1.3.`,
	"degen": `Persona: leverage maximiser. Keep your WETH exposure as high as possible: deposit WETH, borrow USD close to the
limit, buy more WETH, and repeat. Deleverage only when liquidation is imminent.`,
	"panic": `Persona: loss-averse trader. Your priority is to avoid losses. If the price drops noticeably or your position looks
risky, sell WETH for USD and repay debt immediately; buy back only after the price has clearly recovered.`,
}

func (w *world) setT0(t0 time.Time) {
	w.t0 = t0
	for _, b := range w.bases {
		b.T0 = t0
		if b.C != w.deployer { // the oracle reporter must not lag the price by several blocks
			b.C.Confirmations = w.c.Confirmations
		}
	}
}

// note records a simulation-level event (faults), shown in the log and kept for the summary.
func (w *world) note(kind, detail string) {
	e := agent.Event{T: time.Since(w.t0).Seconds(), Agent: "sim", Kind: kind, Detail: detail}
	w.log.Printf("*** %s: %s", kind, detail)
	w.mu.Lock()
	w.notes = append(w.notes, e)
	w.mu.Unlock()
	select {
	case w.events <- e:
	default:
	}
}

// outputs opens the CSV files; without an output directory they are discarded.
func (w *world) outputs() (ts, pos io.Writer, closeAll func(), err error) {
	if w.c.Out == "" {
		return io.Discard, io.Discard, func() {}, nil
	}
	if err := os.MkdirAll(w.c.Out, 0o755); err != nil {
		return nil, nil, nil, err
	}
	tf, err := os.Create(filepath.Join(w.c.Out, "timeseries.csv"))
	if err != nil {
		return nil, nil, nil, err
	}
	pf, err := os.Create(filepath.Join(w.c.Out, "positions.csv"))
	if err != nil {
		tf.Close()
		return nil, nil, nil, err
	}
	return tf, pf, func() { tf.Close(); pf.Close() }, nil
}
