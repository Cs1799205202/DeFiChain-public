package agent

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/client"
)

// Sample is one row of the time series.
type Sample struct {
	T      float64 // seconds since run start
	Height uint64
	Miners int     // miner nodes alive
	Reorgs int     // reorgs so far (the most any node has seen)
	Truth  float64 // real-world price
	// Fundamental is the scripted price before trade impact; Impact the log shift trades
	// on the exchange caused (Truth = Fundamental * e^Impact).
	Fundamental float64
	Impact      float64
	Oracle      float64 // on-chain feed price
	Pool        float64 // AMM spot price
	LendOracle  float64 // the price the lending market actually uses (feed or AMM spot)
	Debt        float64 // total debt across borrowers
	Collateral  float64 // total collateral (WETH)
	BadDebt     float64 // sum over borrowers of max(0, debt - collateral*truth)
	Unhealthy   int     // positions the lending market considers liquidatable
	Insolvent   int     // positions whose collateral is worth less than their debt at truth
}

// Position is one borrower's state, as read by the recorder.
type Position struct {
	Who        common.Address
	Debt       float64
	Collateral float64
	Health     float64 // on-chain health factor
}

// Recorder samples the market every Every and writes CSV rows.
type Recorder struct {
	C      *client.Client
	M      *Market
	Truth  *Truth
	Every  time.Duration
	T0     time.Time
	Miners func() int
	// Reorgs reports reorgs so far and the deepest one (optional).
	Reorgs func() (count, maxDepth int)
	Out    io.Writer
	// PosOut, if set, receives one row per borrower per sample: t,address,debt,collateral,health.
	PosOut  io.Writer
	Samples []Sample
	posW    *csv.Writer
}

var sampleHeader = []string{"t", "height", "miners", "reorgs", "truth", "oracle", "pool", "lend_oracle", "debt", "collateral", "bad_debt", "unhealthy", "insolvent", "fundamental", "impact"}

func (r *Recorder) Run(ctx context.Context) {
	w := csv.NewWriter(r.Out)
	w.Write(sampleHeader)
	if r.PosOut != nil {
		r.posW = csv.NewWriter(r.PosOut)
		r.posW.Write([]string{"t", "address", "debt", "collateral", "health"})
	}
	for {
		if s, err := r.sample(ctx); err == nil {
			r.Samples = append(r.Samples, s)
			w.Write(s.row())
			w.Flush()
		}
		if !sleep(ctx, r.Every) {
			return
		}
	}
}

func (s Sample) row() []string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', 4, 64) }
	return []string{f(s.T), fmt.Sprint(s.Height), fmt.Sprint(s.Miners), fmt.Sprint(s.Reorgs), f(s.Truth), f(s.Oracle), f(s.Pool), f(s.LendOracle),
		f(s.Debt), f(s.Collateral), f(s.BadDebt), fmt.Sprint(s.Unhealthy), fmt.Sprint(s.Insolvent), f(s.Fundamental),
		strconv.FormatFloat(s.Impact, 'f', 6, 64)}
}

func (r *Recorder) sample(ctx context.Context) (Sample, error) {
	// Short deadline: during an outage reads may fail, and the sample is then skipped
	// except for the fields that do not need the chain.
	rctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	s := Sample{T: time.Since(r.T0).Seconds(), Truth: r.Truth.Price(), Fundamental: r.Truth.Fundamental(), Impact: r.Truth.Impact()}
	if r.Miners != nil {
		s.Miners = r.Miners()
	}
	if r.Reorgs != nil {
		s.Reorgs, _ = r.Reorgs()
	}
	snap, err := r.M.Read(rctx, r.C)
	if err != nil {
		return s, err
	}
	s.Height, s.Oracle, s.Pool = snap.Block, snap.FeedPrice, snap.PoolPrice
	if lo, err := r.C.ViewBig(rctx, client.AMMOracleContract, r.M.LendingOracle, "price"); err == nil {
		s.LendOracle = F(lo)
	}
	positions, err := Positions(rctx, r.C, r.M)
	if err != nil {
		return s, err
	}
	for _, p := range positions {
		if r.posW != nil {
			health := "inf"
			if !math.IsInf(p.Health, 1) {
				health = strconv.FormatFloat(p.Health, 'f', 4, 64)
			}
			r.posW.Write([]string{strconv.FormatFloat(s.T, 'f', 2, 64), p.Who.Hex(), strconv.FormatFloat(p.Debt, 'f', 2, 64),
				strconv.FormatFloat(p.Collateral, 'f', 4, 64), health})
		}
		s.Debt += p.Debt
		s.Collateral += p.Collateral
		if short := p.Debt - p.Collateral*s.Truth; short > 0 {
			s.BadDebt += short
			s.Insolvent++
		}
		if p.Debt > 0 && p.Health < 1 {
			s.Unhealthy++
		}
	}
	if r.posW != nil {
		r.posW.Flush()
	}
	return s, nil
}

// Positions reads every borrower's position from the lending market.
func Positions(ctx context.Context, c *client.Client, m *Market) ([]Position, error) {
	n, err := c.ViewBig(ctx, client.LendingContract, m.Lending, "borrowersCount")
	if err != nil {
		return nil, err
	}
	var out []Position
	for i := int64(0); i < n.Int64(); i++ {
		res, err := c.View(ctx, client.LendingContract, m.Lending, "borrowers", big.NewInt(i))
		if err != nil {
			return nil, err
		}
		who := res[0].(common.Address)
		debt, err := c.ViewBig(ctx, client.LendingContract, m.Lending, "debtOf", who)
		if err != nil {
			return nil, err
		}
		coll, err := c.ViewBig(ctx, client.LendingContract, m.Lending, "collateralOf", who)
		if err != nil {
			return nil, err
		}
		hf, err := c.ViewBig(ctx, client.LendingContract, m.Lending, "healthFactor", who)
		if err != nil {
			return nil, err
		}
		health := math.Inf(1)
		if hf.Cmp(MaxUint) != 0 {
			health = F(hf)
		}
		out = append(out, Position{Who: who, Debt: F(debt), Collateral: F(coll), Health: health})
	}
	return out, nil
}
