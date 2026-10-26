// Command sim runs a whole DeFi economy: a PoW devnet (separate node processes), the
// Token / AMM / Oracle / Lending contracts, a "real world" price on an off-chain exchange,
// and a population of agents (scripted bots, LLM traders, and your own agents in any
// language, launched as subprocesses).
//
//	go run ./cmd/sim                                     # 60 s, scripted agents, 30% crash at 25 s
//	go run ./cmd/sim -llm 2                              # plus two LLM traders (needs LLM_API_KEY)
//	go run ./cmd/sim -agent "me=python3 pysdk/examples/momentum.py"   # plus your own agent
//	go run ./cmd/sim -miners 4 -delay 800ms -jitter 800ms             # slow network: forks, reorgs
//	go run ./cmd/sim -partition-at 30s -partition-for 20s             # split the miners for 20 s
//	go run ./cmd/sim -out results/run1                   # write CSV/JSON for plotting
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"defichain/agent"
	"defichain/client"
	"defichain/devnet"
	"defichain/spec"
)

type config struct {
	Miners         int           `json:"miners"`
	Hashrate       string        `json:"hashrate"`
	Port           int           `json:"port"`
	Delay          time.Duration `json:"delay"`
	Jitter         time.Duration `json:"jitter"`
	Drop           float64       `json:"drop"`
	PartitionAt    time.Duration `json:"partitionAt"`
	PartitionFor   time.Duration `json:"partitionFor"`
	Confirmations  uint64        `json:"confirmations"`
	Duration       time.Duration `json:"duration"`
	Seed           int64         `json:"seed"`
	Price          float64       `json:"price"`
	Volatility     float64       `json:"volatility"`
	CrashAt        time.Duration `json:"crashAt"`
	CrashFor       time.Duration `json:"crashFor"`
	CrashPct       float64       `json:"crashPct"`
	Rebound        float64       `json:"rebound"`
	ReboundFor     time.Duration `json:"reboundFor"`
	OracleEvery    time.Duration `json:"oracleEvery"`
	ArbRef         string        `json:"arbRef"`
	PoolWETH       float64       `json:"poolWETH"`
	Borrowers      int           `json:"borrowers"`
	LTVMin         float64       `json:"ltvMin"`
	LTVMax         float64       `json:"ltvMax"`
	Noise          int           `json:"noise"`
	Liquidators    int           `json:"liquidators"`
	LLM            int           `json:"llm"`
	LLMEvery       time.Duration `json:"llmEvery"`
	LLMMix         string        `json:"llmMix"`
	LLMTape        bool          `json:"llmTape"`
	ExchangeDepth  float64       `json:"exchangeDepth"`
	ImpactHalfLife time.Duration `json:"impactHalfLife"`
	Managed        bool          `json:"managed"`
	ManagedLTV     float64       `json:"managedLTV"`
	SpotOracle     bool          `json:"spotOracle"`
	NoArb          bool          `json:"noArb"`
	Agents         []string      `json:"agents"` // external agents: "name=command line"
	AgentUSD       float64       `json:"agentUSD"`
	AgentWETH      float64       `json:"agentWETH"`
	ExchangePort   int           `json:"exchangePort"`
	Out            string        `json:"out"`
}

type agentFlags []string

func (a *agentFlags) String() string     { return strings.Join(*a, "; ") }
func (a *agentFlags) Set(v string) error { *a = append(*a, v); return nil }

func main() {
	var c config
	var agents agentFlags
	flag.IntVar(&c.Miners, "miners", 3, "number of mining nodes")
	flag.StringVar(&c.Hashrate, "hashrate", "200000", "hashrate cap per miner in H/s, comma-separated (last value repeats)")
	flag.IntVar(&c.Port, "port", 28000, "node i listens on port+i")
	flag.DurationVar(&c.Delay, "delay", 0, "p2p delay on every message (more delay: more forks and reorgs)")
	flag.DurationVar(&c.Jitter, "jitter", 0, "extra random p2p delay in [0, jitter)")
	flag.Float64Var(&c.Drop, "drop", 0, "probability that a pushed block or transaction is lost")
	flag.DurationVar(&c.PartitionAt, "partition-at", 0, "split the miners into two halves at this time (0 = never)")
	flag.DurationVar(&c.PartitionFor, "partition-for", 20*time.Second, "how long the partition lasts")
	flag.Uint64Var(&c.Confirmations, "confirmations", 0, "blocks the built-in agents wait on top of their transactions")
	flag.DurationVar(&c.Duration, "duration", 60*time.Second, "how long the agents run")
	flag.Int64Var(&c.Seed, "seed", 1, "random seed")
	flag.Float64Var(&c.Price, "price", 2000, "initial USD per WETH")
	flag.Float64Var(&c.Volatility, "volatility", 0.004, "exchange price volatility per sqrt(second)")
	flag.DurationVar(&c.CrashAt, "crash-at", 25*time.Second, "when the exchange price starts to fall (0 = no crash)")
	flag.DurationVar(&c.CrashFor, "crash-for", 20*time.Second, "how long the fall takes")
	flag.Float64Var(&c.CrashPct, "crash-pct", 0.3, "size of the fall")
	flag.Float64Var(&c.Rebound, "rebound", 0, "after the fall, recover this fraction of the start price (V-shaped crash)")
	flag.DurationVar(&c.ReboundFor, "rebound-for", 20*time.Second, "how long the recovery takes")
	flag.DurationVar(&c.OracleEvery, "oracle-every", time.Second, "oracle reporter heartbeat")
	flag.StringVar(&c.ArbRef, "arb-ref", "truth", "arbitrage reference price: truth or feed")
	flag.BoolVar(&c.NoArb, "no-arb", false, "run without the arbitrage bot")
	flag.Float64Var(&c.PoolWETH, "pool-weth", 500, "initial AMM depth in WETH")
	flag.IntVar(&c.Borrowers, "borrowers", 4, "number of unmanaged leveraged borrowers")
	flag.Float64Var(&c.LTVMin, "ltv-min", 0.62, "lowest borrower LTV")
	flag.Float64Var(&c.LTVMax, "ltv-max", 0.71, "highest borrower LTV")
	flag.IntVar(&c.Noise, "noise", 3, "number of noise traders")
	flag.IntVar(&c.Liquidators, "liquidators", 1, "number of competing liquidators")
	flag.IntVar(&c.LLM, "llm", 0, "number of built-in LLM traders (needs LLM_API_KEY or LLM_API_KEY_FILE)")
	flag.DurationVar(&c.LLMEvery, "llm-every", 6*time.Second, "LLM decision interval")
	flag.StringVar(&c.LLMMix, "llm-mix", strings.Join(personaOrder, ","), "comma-separated personas, cycled over the LLM traders ("+strings.Join(personaOrder, ", ")+")")
	flag.BoolVar(&c.LLMTape, "llm-tape", false, "show LLM traders a summary of everyone else's recent trades and liquidations")
	flag.Float64Var(&c.ExchangeDepth, "exchange-depth", 0, "exchange depth in WETH: trading q WETH there moves the log price by q/depth (0 = infinitely deep)")
	flag.DurationVar(&c.ImpactHalfLife, "impact-halflife", 30*time.Second, "how fast the exchange recovers from trade impact")
	flag.BoolVar(&c.Managed, "managed", false, "add three identical leveraged longs: passive, rule-based and LLM risk manager")
	flag.Float64Var(&c.ManagedLTV, "managed-ltv", 0.68, "LTV of the managed positions")
	flag.BoolVar(&c.SpotOracle, "spot-oracle", false, "lending uses the AMM spot price as its oracle")
	flag.Var(&agents, "agent", `external agent "name=command args..." (repeatable); see docs/AGENTS.md`)
	flag.Float64Var(&c.AgentUSD, "agent-usd", 100_000, "USD each external agent starts with")
	flag.Float64Var(&c.AgentWETH, "agent-weth", 50, "WETH each external agent starts with")
	flag.IntVar(&c.ExchangePort, "exchange-port", 0, "port of the exchange API for external agents (0 = port+99)")
	flag.StringVar(&c.Out, "out", "", "output directory (default results/run-<date>-<time>): CSVs, JSON, agent and node logs")
	flag.Parse()
	c.Agents = agents
	if c.Out == "" {
		c.Out = filepath.Join("results", "run-"+time.Now().Format("20060102-150405"))
	}
	if c.ExchangePort == 0 {
		c.ExchangePort = c.Port + 99
	}
	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

func run(c config) error {
	logger := log.New(os.Stdout, "", log.Ltime)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	externals, err := parseExternal(c.Agents, plannedAgents(c))
	if err != nil {
		return err
	}
	rates, err := parseRates(c.Hashrate)
	if err != nil {
		return err
	}
	// Every account the simulation uses gets gas money at genesis.
	gas := map[string]bool{"deployer": true, "recorder": true, "sink": true}
	for _, name := range plannedAgents(c) {
		gas[name] = true
	}
	for _, e := range externals {
		gas[e.name] = true
	}
	g := devnet.DevGenesis(c.Miners, rates, nil)
	for name := range gas {
		g.Alloc[client.AddressFor(name).Hex()] = spec.Account{Balance: spec.NewBig(client.Ether(1_000))}
	}
	dir := filepath.Join(c.Out, "devnet")
	d, err := devnet.Start(devnet.Options{N: c.Miners, Hashrate: rates, Mine: true, BasePort: c.Port, Genesis: g, Dir: dir,
		Net: spec.NetConditions{Delay: spec.Duration(c.Delay), Jitter: spec.Duration(c.Jitter), Drop: c.Drop}})
	if err != nil {
		return err
	}
	defer d.Close()
	logger.Printf("devnet up: %d miners (%s H/s), target block time %ds, logs in %s", c.Miners, c.Hashrate, g.Config.TargetBlockTime, d.Dir)

	deployer := client.NewFromName("deployer", d.URLs()...)
	market, err := agent.Setup(ctx, deployer, agent.SetupParams{
		Price: c.Price, PoolWETH: c.PoolWETH, LendingUSD: 3_000_000, InterestPerS: big.NewInt(1e12), UseSpotOracle: c.SpotOracle,
	})
	if err != nil {
		return fmt.Errorf("deploy market: %w", err)
	}
	logger.Printf("market deployed: pool %s, lending %s", market.Pool.Hex(), market.Lending.Hex())

	events := make(chan agent.Event, 1<<16)
	truth := agent.NewTruth(c.Price)
	truth.Depth, truth.HalfLife = c.ExchangeDepth, c.ImpactHalfLife
	w := &world{c: c, net: d, market: market, deployer: deployer, truth: truth, events: events, log: logger,
		personas: map[string]string{}, tape: &agent.Tape{}, external: externals, exchange: newExchange(truth)}
	for _, e := range externals {
		w.exchange.register(e)
	}
	if err := w.build(ctx); err != nil {
		return err
	}
	logger.Printf("setup done: %d agents funded", len(w.agents)+len(externals))

	worldPath, err := w.writeWorld()
	if err != nil {
		return err
	}
	stopExchange, err := w.exchange.serve(c.ExchangePort)
	if err != nil {
		return err
	}
	defer stopExchange()

	// Everything below is timed from t0: the crash, the partition and the samples.
	t0 := time.Now()
	w.setT0(t0)
	runCtx, cancel := context.WithTimeout(ctx, c.Duration)
	defer cancel()
	go truth.Run(runCtx, t0, agent.PricePath{Start: c.Price, Volatility: c.Volatility, CrashAt: c.CrashAt, CrashFor: c.CrashFor,
		CrashDepth: c.CrashPct, Rebound: c.Rebound, ReboundFor: c.ReboundFor, Rand: rand.New(rand.NewSource(c.Seed))})
	go w.faults(runCtx)

	ts, pos, closeOut, err := w.outputs()
	if err != nil {
		return err
	}
	defer closeOut()
	rec := &agent.Recorder{C: client.NewFromName("recorder", d.URLs()...), M: market, Truth: truth, Every: time.Second, T0: t0,
		Miners: func() int { return len(d.Alive()) }, Reorgs: w.reorgCount, Out: ts, PosOut: pos}

	var wg sync.WaitGroup
	for _, a := range w.agents {
		wg.Add(1)
		go func() { defer wg.Done(); a.Run(runCtx) }()
	}
	for _, e := range externals {
		wg.Add(1)
		go func() { defer wg.Done(); w.runExternal(runCtx, e, worldPath) }()
	}
	recDone := make(chan struct{})
	go func() { rec.Run(runCtx); close(recDone) }()

	var all []agent.Event
	collected := make(chan struct{})
	go func() {
		for e := range events {
			w.tape.Add(e)
			all = append(all, e)
		}
		close(collected)
	}()
	wg.Wait()
	<-recDone
	close(events)
	<-collected

	sum := w.summarize(ctx, all, rec.Samples)
	w.print(sum)
	logger.Printf("results in %s (plot: python3 pysdk/examples/plot_run.py %s)", c.Out, c.Out)
	if c.Out != "" {
		if err := writeJSON(filepath.Join(c.Out, "events.json"), all); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(c.Out, "summary.json"), sum); err != nil {
			return err
		}
	}
	return nil
}

// faults runs the scheduled partition.
func (w *world) faults(ctx context.Context) {
	if w.c.PartitionAt <= 0 || w.c.Miners < 2 {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(w.c.PartitionAt):
	}
	half := len(w.net.Nodes) / 2
	a, b := w.net.Nodes[:half], w.net.Nodes[half:]
	if err := w.net.Partition(a, b); err != nil {
		w.note("fault-error", err.Error())
		return
	}
	w.note("partition", fmt.Sprintf("miners split into %s | %s for %s", ids(a), ids(b), w.c.PartitionFor))
	select {
	case <-ctx.Done():
	case <-time.After(w.c.PartitionFor):
	}
	if err := w.net.Heal(); err != nil {
		w.note("fault-error", err.Error())
		return
	}
	w.note("heal", "partition healed")
}

func ids(nodes []*devnet.Node) string {
	var s []string
	for _, n := range nodes {
		s = append(s, n.ID)
	}
	return strings.Join(s, ",")
}

// reorgCount is the most reorgs any node has seen, and the deepest one.
func (w *world) reorgCount() (count, maxDepth int) {
	for _, n := range w.net.Alive() {
		r, err := n.Reorgs()
		if err != nil {
			continue
		}
		count = max(count, r.Count)
		for _, e := range r.Recent {
			maxDepth = max(maxDepth, e.Removed)
		}
	}
	return
}

func parseRates(s string) ([]float64, error) {
	var out []float64
	for _, f := range strings.Split(s, ",") {
		var v float64
		if _, err := fmt.Sscan(strings.TrimSpace(f), &v); err != nil {
			return nil, fmt.Errorf("bad -hashrate %q", s)
		}
		out = append(out, v)
	}
	return out, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
