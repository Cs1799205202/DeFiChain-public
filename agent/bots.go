package agent

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/client"
)

// Agent is anything that acts on the market until ctx is cancelled.
type Agent interface {
	Name() string
	Account() common.Address
	Run(ctx context.Context)
}

// Base carries what every agent needs.
type Base struct {
	ID     string
	C      *client.Client
	M      *Market
	Log    *log.Logger
	Events chan<- Event
	T0     time.Time // run start, for event timestamps
}

func (b *Base) Name() string            { return b.ID }
func (b *Base) Account() common.Address { return b.C.Account }

// Event is a notable agent action. Data holds numeric fields for analysis.
type Event struct {
	T      float64            `json:"t"` // seconds since run start
	Block  uint64             `json:"block"`
	Agent  string             `json:"agent"`
	Kind   string             `json:"kind"`
	Detail string             `json:"detail"`
	Value  float64            `json:"value,omitempty"`
	Data   map[string]float64 `json:"data,omitempty"`
	User   string             `json:"user,omitempty"` // the account acted upon, e.g. a liquidated borrower
}

func (b *Base) emit(kind, detail string, value float64) { b.emitData(kind, detail, value, nil) }

func (b *Base) emitData(kind, detail string, value float64, data map[string]float64) {
	b.emitFull(Event{Kind: kind, Detail: detail, Value: value, Data: data})
}

func (b *Base) emitFull(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	height, _ := b.C.BlockNumber(ctx)
	cancel()
	b.Log.Printf("%-11s %-13s %s", b.ID, e.Kind, e.Detail)
	e.Block, e.Agent = height, b.ID
	if !b.T0.IsZero() {
		e.T = time.Since(b.T0).Seconds() // setup events, before the clock starts, stay at 0
	}
	if b.Events != nil {
		select {
		case b.Events <- e:
		default:
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ---------------------------------------------------------------------------------------
// Oracle reporter: every Every, read the truth price and push it on-chain. While the chain
// is down its transaction just waits, so the on-chain price goes stale.

type Reporter struct {
	*Base
	Truth *Truth
	Every time.Duration
}

func (r *Reporter) Run(ctx context.Context) {
	for sleep(ctx, r.Every) {
		p := r.Truth.Price()
		if _, err := r.C.Transact(ctx, client.FeedOracleContract, r.M.Feed, "setPrice", E18(p)); err != nil {
			continue
		}
		r.emitData("price", formatf("feed -> %.2f (truth now %.2f)", p, r.Truth.Price()), p, map[string]float64{"price": p})
	}
}

// ---------------------------------------------------------------------------------------
// Arbitrageur: trades the pool toward a reference price and pockets the gap. A real
// arbitrageur hedges on an exchange, so its reference is the truth price; set Truth to nil
// to arbitrage against the on-chain oracle instead.

type Arbitrageur struct {
	*Base
	Truth   *Truth
	MinEdge float64 // minimum relative gap before trading, e.g. 0.006
	Every   time.Duration
	// Hedge offsets every pool trade on the exchange (buy WETH cheap on the pool, sell the
	// same amount on the exchange), so the arbitrageur carries no inventory risk. With a
	// finite-depth exchange this is how pool selling pressure reaches the exchange price.
	Hedge bool
	// The exchange account the hedges settle in (it may go negative in either token).
	ExUSD, ExWETH float64
	mu            sync.Mutex
}

// OffChainValue is the exchange account's worth at price p, for net worth accounting.
func (a *Arbitrageur) OffChainValue(p float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExUSD + a.ExWETH*p
}

func (a *Arbitrageur) Run(ctx context.Context) {
	for sleep(ctx, a.Every) {
		s, err := a.M.Read(ctx, a.C)
		if err != nil {
			continue
		}
		ref := s.FeedPrice
		if a.Truth != nil {
			ref = a.Truth.Price()
		}
		gap := (ref - s.PoolPrice) / s.PoolPrice
		if math.Abs(gap) < a.MinEdge {
			continue
		}
		buyWETH, amount := ArbAmountIn(s, ref)
		h, err := a.M.Holdings(ctx, a.C, a.Account())
		if err != nil {
			continue
		}
		tokenIn, side := a.M.USD, "buy WETH"
		if buyWETH {
			amount = math.Min(amount, h.USD*0.99)
		} else {
			tokenIn, side = a.M.WETH, "sell WETH"
			amount = math.Min(amount, h.WETH*0.99)
		}
		if amount <= 0 {
			continue
		}
		before := h.NetWorth(ref)
		if _, err := a.C.Transact(ctx, client.AMMContract, a.M.Pool, "swap", tokenIn, E18(amount), E18(0)); err != nil {
			continue
		}
		after, err := a.M.Holdings(ctx, a.C, a.Account())
		if err != nil {
			continue
		}
		dWETH, dUSD := after.WETH-h.WETH, after.USD-h.USD
		profit := after.NetWorth(ref) - before
		data := map[string]float64{"gap": gap, "pnl": profit, "weth": dWETH, "usd": dUSD}
		if a.Hedge && a.Truth != nil && dWETH != 0 {
			// Undo the WETH position on the exchange: sell what the pool sold us, or buy
			// back what we sold into the pool.
			px := a.Truth.Trade(-dWETH)
			a.mu.Lock()
			a.ExWETH -= dWETH
			a.ExUSD += dWETH * px
			a.mu.Unlock()
			profit = after.NetWorth(ref) - before + dWETH*(px-ref)
			data["hedgePrice"], data["pnl"] = px, profit
		}
		a.emitData("arbitrage", formatf("%s, pool %.2f vs ref %.2f (gap %+.2f%%), pnl %+.2f", side, s.PoolPrice, ref, gap*100, profit), profit, data)
	}
}

// ---------------------------------------------------------------------------------------
// Liquidator: scans every borrower and liquidates unhealthy positions when it is profitable,
// then sells the seized collateral into the pool. It values collateral at the truth price,
// because that is what it can really sell it for.

type Liquidator struct {
	*Base
	Truth *Truth
	Every time.Duration
	// KeepCollateral skips selling seized WETH into the pool.
	KeepCollateral bool
	// Rand, if set, shuffles the scan order, so competing liquidators do not all go for
	// the same borrower first.
	Rand *rand.Rand
}

func (l *Liquidator) Run(ctx context.Context) {
	for sleep(ctx, l.Every) {
		n, err := l.C.ViewBig(ctx, client.LendingContract, l.M.Lending, "borrowersCount")
		if err != nil {
			continue
		}
		order := make([]int64, n.Int64())
		for i := range order {
			order[i] = int64(i)
		}
		if l.Rand != nil {
			l.Rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		}
		for _, i := range order {
			out, err := l.C.View(ctx, client.LendingContract, l.M.Lending, "borrowers", big.NewInt(i))
			if err != nil {
				continue
			}
			l.try(ctx, out[0].(common.Address))
		}
	}
}

func (l *Liquidator) try(ctx context.Context, user common.Address) {
	hf, err := l.C.ViewBig(ctx, client.LendingContract, l.M.Lending, "healthFactor", user)
	if err != nil || hf.Cmp(E18(1)) >= 0 {
		return
	}
	pos, err := l.M.Holdings(ctx, l.C, user)
	if err != nil || pos.Debt <= 0 {
		return
	}
	s, err := l.M.Read(ctx, l.C)
	if err != nil {
		return
	}
	truth := s.FeedPrice
	if l.Truth != nil {
		truth = l.Truth.Price()
	}
	mine, _ := l.M.Holdings(ctx, l.C, l.Account())
	// Repay at most the close factor, no more than the collateral can pay for (seizure is
	// capped at the position's collateral), and no more than we hold.
	repay := math.Min(pos.Debt*0.5, pos.Collateral*s.LendPrice/1.05*0.999)
	repay = math.Min(repay, mine.USD*0.99)
	if repay <= 1 {
		return
	}
	seize := math.Min(repay*1.05/s.LendPrice, pos.Collateral)
	profit := seize*truth - repay
	if profit <= 0 {
		l.emitData("liq-skip", formatf("%s: unprofitable (oracle %.0f, truth %.0f)", short(user), s.LendPrice, truth), 0,
			map[string]float64{"profit": profit})
		return
	}
	healthAtTruth := pos.Collateral * truth * 0.8 / pos.Debt
	r, err := l.C.Transact(ctx, client.LendingContract, l.M.Lending, "liquidate", user, E18(repay))
	if err != nil {
		l.emit("liq-miss", formatf("%s: %v", short(user), err), 0)
		return
	}
	// Read the actual (seized, paid) from the Liquidate event, not from our estimate: when
	// liquidators race and the seizure is capped by the remaining collateral, the contract
	// scales the payment down, and only the event says by how much.
	var actualSeized, actualPaid float64
	var seizedRaw *big.Int // exact: selling E18(actualSeized) could round above what we got
	for _, ev := range client.LendingContract.Events(r.Logs, l.M.Lending, "Liquidate") {
		seizedRaw = ev["seized"].(*big.Int)
		actualSeized, actualPaid = F(seizedRaw), F(ev["repaid"].(*big.Int))
	}
	if actualSeized == 0 {
		// fallback: derive from holdings diff
		after, _ := l.M.Holdings(ctx, l.C, l.Account())
		actualSeized = after.WETH - mine.WETH
		actualPaid = repay
		if after.RawWETH != nil && mine.RawWETH != nil {
			seizedRaw = new(big.Int).Sub(after.RawWETH, mine.RawWETH)
		}
	}
	l.emitFull(Event{Kind: "liquidate", User: user.Hex(), Value: actualSeized*truth - actualPaid,
		Detail: formatf("%s oracle-health %.3f truth-health %.3f: repaid %.0f, seized %.3f WETH (≈%+.0f)",
			short(user), F(hf), healthAtTruth, actualPaid, actualSeized, actualSeized*truth-actualPaid),
		Data: map[string]float64{"repaid": actualPaid, "seized": actualSeized, "oracleHealth": F(hf), "truthHealth": healthAtTruth,
			"oracle": s.LendPrice, "truth": truth, "penalty": actualSeized*truth - actualPaid}})
	if actualSeized > 0 && seizedRaw != nil && seizedRaw.Sign() > 0 && !l.KeepCollateral {
		r, err := l.C.Transact(ctx, client.AMMContract, l.M.Pool, "swap", l.M.WETH, seizedRaw, E18(0))
		if err != nil {
			l.emit("dump-failed", formatf("sell %.3f seized WETH: %v", actualSeized, err), 0)
		} else {
			got := swapOut(r, l.M.Pool)
			l.emitData("dump", formatf("sold %.3f seized WETH into the pool for %.0f USD (%.2f each, truth %.2f)",
				actualSeized, got, got/actualSeized, truth), got, map[string]float64{"weth": -actualSeized, "usd": got})
		}
	}
}

// ---------------------------------------------------------------------------------------
// Noise trader: random retail swaps, so the pool drifts and there is flow.

type NoiseTrader struct {
	*Base
	MaxUSD float64
	Every  time.Duration
	Rand   *rand.Rand
}

func (n *NoiseTrader) Run(ctx context.Context) {
	for sleep(ctx, time.Duration(float64(n.Every)*(0.5+n.Rand.Float64()))) {
		s, err := n.M.Read(ctx, n.C)
		if err != nil {
			continue
		}
		usd := n.MaxUSD * (0.2 + 0.8*n.Rand.Float64())
		tokenIn, amount, side := n.M.USD, usd, "buy WETH"
		if n.Rand.Intn(2) == 0 {
			tokenIn, amount, side = n.M.WETH, usd/s.PoolPrice, "sell WETH"
		}
		// 1% slippage tolerance, a typical wallet default.
		var minOut float64
		if tokenIn == n.M.USD {
			minOut = F(AmountOut(E18(amount), s.RawUSD, s.RawWETH)) * 0.99
		} else {
			minOut = F(AmountOut(E18(amount), s.RawWETH, s.RawUSD)) * 0.99
		}
		r, err := n.C.Transact(ctx, client.AMMContract, n.M.Pool, "swap", tokenIn, E18(amount), E18(minOut))
		if err != nil {
			continue
		}
		out := swapOut(r, n.M.Pool)
		data := map[string]float64{"weth": out, "usd": -amount}
		if tokenIn == n.M.WETH {
			data = map[string]float64{"weth": -amount, "usd": out}
		}
		n.emitData("swap", formatf("%s ~%.0f USD at pool %.2f", side, usd, s.PoolPrice), usd, data)
	}
}

// ---------------------------------------------------------------------------------------
// Borrower: a leveraged position at TargetLTV that is never managed. The simulation calls
// Open during setup. If SpendTo is set, the borrowed USD is transferred there ("spent", e.g.
// on the WETH the account holds), so it cannot simply repay from its wallet.

type Borrower struct {
	*Base
	Collateral float64
	TargetLTV  float64
	SpendTo    *common.Address
}

// Open deposits collateral and borrows.
func (b *Borrower) Open(ctx context.Context) error {
	s, err := b.M.Read(ctx, b.C)
	if err != nil {
		return err
	}
	if _, err := b.C.Transact(ctx, client.LendingContract, b.M.Lending, "deposit", E18(b.Collateral)); err != nil {
		return err
	}
	borrow := b.Collateral * s.LendPrice * b.TargetLTV
	if _, err := b.C.Transact(ctx, client.LendingContract, b.M.Lending, "borrow", E18(borrow)); err != nil {
		return err
	}
	if b.SpendTo != nil {
		if _, err := b.C.Transact(ctx, client.TokenContract, b.M.USD, "transfer", *b.SpendTo, E18(borrow)); err != nil {
			return err
		}
	}
	b.emitData("borrow", formatf("deposited %.1f WETH, borrowed %.0f USD (LTV %.0f%%)", b.Collateral, borrow, b.TargetLTV*100), borrow,
		map[string]float64{"ltv": b.TargetLTV, "borrow": borrow})
	return nil
}

func (b *Borrower) Run(ctx context.Context) { <-ctx.Done() }

// ---------------------------------------------------------------------------------------
// Deleverager: a rule-based risk manager on a leveraged position (opened during setup).
// When the health factor drops below Trigger it sells wallet WETH and repays debt until the
// health factor reaches Target.

type Deleverager struct {
	Borrower
	Trigger, Target float64
	Every           time.Duration
}

func (d *Deleverager) Run(ctx context.Context) {
	for sleep(ctx, d.Every) {
		h, err := d.M.Holdings(ctx, d.C, d.Account())
		if err != nil || h.Debt <= 0 || h.Health >= d.Trigger {
			continue
		}
		s, err := d.M.Read(ctx, d.C)
		if err != nil {
			continue
		}
		// Debt that would give the target health at the current oracle price.
		want := h.Collateral * s.LendPrice * 0.8 / d.Target
		need := h.Debt - want
		if need <= 0 {
			continue
		}
		if h.USD < need && h.WETH > 0 {
			sell := math.Min(h.WETH*0.999, (need-h.USD)/s.PoolPrice*1.02)
			min := F(AmountOut(E18(sell), s.RawWETH, s.RawUSD)) * 0.97
			if _, err := d.C.Transact(ctx, client.AMMContract, d.M.Pool, "swap", d.M.WETH, E18(sell), E18(min)); err == nil {
				d.emitData("sell", formatf("sold %.3f WETH to repay", sell), sell, map[string]float64{"weth": sell})
			}
			h, _ = d.M.Holdings(ctx, d.C, d.Account())
		}
		repay := math.Min(need, h.USD*0.999)
		if repay <= 0 {
			continue
		}
		if _, err := d.C.Transact(ctx, client.LendingContract, d.M.Lending, "repay", E18(repay)); err != nil {
			continue
		}
		after, _ := d.M.Holdings(ctx, d.C, d.Account())
		d.emitData("repay", formatf("health %.3f -> %.3f, repaid %.0f", h.Health, after.Health, repay), repay,
			map[string]float64{"repaid": repay, "healthBefore": h.Health, "healthAfter": after.Health})
	}
}

// swapOut is the amount a swap paid out, from the pool's Swap event in the receipt.
func swapOut(r *client.Receipt, pool common.Address) float64 {
	for _, ev := range client.AMMContract.Events(r.Logs, pool, "Swap") {
		return F(ev["amountOut"].(*big.Int))
	}
	return 0
}

func formatf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func short(a common.Address) string { return a.Hex()[:8] }
