package agent

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"
)

// Truth is the "real world" WETH price, on a centralised exchange. It moves on its own
// clock whether or not the chain is alive. The on-chain oracle only learns it when the
// reporter's transaction is included, so the two can diverge.
//
// The exchange has finite depth: buying or selling q WETH there moves the log price by
// ±q/Depth, and that impact decays with HalfLife (the market's resilience). Arbitrageurs
// hedge their pool trades here, so selling pressure on the chain (liquidators dumping
// collateral, panicking traders) reaches the exchange price and, through the oracle,
// the lending market: the feedback loop behind liquidation cascades. Depth 0 means an
// infinitely deep exchange whose price nobody on the chain can move.
type Truth struct {
	mu          sync.RWMutex
	fundamental float64 // the scripted path with its noise, before any trade impact
	impact      float64 // log-price shift from exchange trades, decaying
	Depth       float64
	HalfLife    time.Duration
}

// PricePath scripts the fundamental price: a random walk around a baseline that falls
// linearly by CrashDepth over CrashFor, starting CrashAt into the run.
type PricePath struct {
	Start      float64       // initial price
	Volatility float64       // relative stdev per sqrt(second)
	CrashAt    time.Duration // when the crash starts (0 = no crash)
	CrashFor   time.Duration // how long the fall takes
	CrashDepth float64       // e.g. 0.35 for a 35% fall
	// After the fall, the baseline recovers by Rebound (a fraction of Start) over ReboundFor.
	Rebound    float64
	ReboundFor time.Duration
	Rand       *rand.Rand
}

func NewTruth(start float64) *Truth { return &Truth{fundamental: start, HalfLife: 30 * time.Second} }

// Price returns the current exchange price, including trade impact.
func (t *Truth) Price() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.fundamental * math.Exp(t.impact)
}

// Fundamental is the price the exchange would show without the impact of trades.
func (t *Truth) Fundamental() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.fundamental
}

// Impact is the current log-price shift caused by trades (negative: pushed down).
func (t *Truth) Impact() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.impact
}

// Trade buys (q > 0) or sells (q < 0) q WETH on the exchange and returns the average
// execution price. With infinite depth it executes at the current price, without impact.
func (t *Truth) Trade(q float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.fundamental * math.Exp(t.impact)
	if t.Depth <= 0 || q == 0 {
		return p
	}
	// Walking the order book: the log price moves linearly in q, so the average price
	// is p * (e^(q/D) - 1) / (q/D).
	x := q / t.Depth
	t.impact += x
	return p * math.Expm1(x) / x
}

// Set overrides the fundamental price, for a truth process driven from outside Run.
func (t *Truth) Set(p float64) {
	t.mu.Lock()
	t.fundamental = p
	t.mu.Unlock()
}

// Run advances the fundamental price every 250 ms until ctx ends, and lets the trade
// impact decay. t0 is the run's start.
func (t *Truth) Run(ctx context.Context, t0 time.Time, p PricePath) {
	const step = 250 * time.Millisecond
	noise := 1.0
	decay := 1.0
	if t.HalfLife > 0 {
		decay = math.Pow(0.5, step.Seconds()/t.HalfLife.Seconds())
	}
	for sleep(ctx, step) {
		elapsed := time.Since(t0)
		noise *= math.Exp(p.Volatility*math.Sqrt(step.Seconds())*p.Rand.NormFloat64() - 0.5*p.Volatility*p.Volatility*step.Seconds())
		base := p.Start
		if p.CrashAt > 0 && p.CrashDepth > 0 && elapsed > p.CrashAt { // CrashAt 0 means no crash
			frac := 1.0
			if p.CrashFor > 0 {
				frac = math.Min(1, float64(elapsed-p.CrashAt)/float64(p.CrashFor))
			}
			base *= 1 - p.CrashDepth*frac
			if end := p.CrashAt + p.CrashFor; p.Rebound > 0 && elapsed > end {
				up := 1.0
				if p.ReboundFor > 0 {
					up = math.Min(1, float64(elapsed-end)/float64(p.ReboundFor))
				}
				base += p.Start * p.Rebound * up
			}
		}
		t.mu.Lock()
		t.fundamental = base * noise
		t.impact *= decay
		t.mu.Unlock()
	}
}
