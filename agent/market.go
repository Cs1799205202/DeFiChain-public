package agent

import (
	"context"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"defichain/client"
)

// Market is the set of deployed contracts. Pool token0 is WETH and token1 is USD, so the
// pool price and the oracle price are both "USD per WETH".
type Market struct {
	USD     common.Address `json:"usd"`
	WETH    common.Address `json:"weth"`
	Pool    common.Address `json:"pool"`
	Feed    common.Address `json:"feed"`
	Spot    common.Address `json:"spotOracle"`
	Lending common.Address `json:"lending"`
	// LendingOracle is whichever oracle the lending market was deployed with.
	LendingOracle common.Address `json:"lendingOracle"`
}

// Address is the account of the named agent: the address of client.KeyFor(name), the key
// its client signs with.
func Address(name string) common.Address { return client.AddressFor(name) }

// E18 converts a float amount of tokens to its 18-decimal integer form, at a resolution of
// 1e-6 tokens. It goes through exact integer micro-units: a 53-bit float times 1e18 rounds at
// the wei level, so E18(a+b) could differ from E18(a)+E18(b) and a transfer of exactly what
// was minted could fail.
func E18(f float64) *big.Int {
	micro := new(big.Int).SetInt64(int64(math.Round(f * 1e6)))
	return micro.Mul(micro, big.NewInt(1e12))
}

// F converts an 18-decimal integer to a float, for display and strategy maths.
func F(x *big.Int) float64 {
	if x == nil {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(x), big.NewFloat(1e18)).Float64()
	return f
}

// MaxUint is type(uint256).max, used for unlimited approvals.
var MaxUint = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// SetupParams sizes the initial market.
type SetupParams struct {
	Price      float64 // initial USD per WETH
	PoolWETH   float64 // initial pool depth in WETH
	LendingUSD float64 // USD liquidity seeded into the lending market
	// InterestPerS is the borrow rate per second as an 18-decimal integer.
	// E.g. for 1e-6 /s (≈3.1% APR) pass big.NewInt(1e12), which is 1e-6 * 1e18.
	// Cannot use E18(f) for small rates because E18 has 1e-6 resolution and rounds to 0.
	InterestPerS  *big.Int
	UseSpotOracle bool // lending reads the AMM spot price instead of the feed (the flawed variant)
}

// Setup deploys the contracts from the deployer account, which also becomes the oracle
// reporter and the token owner (so it can fund agents later). The six deployments are sent
// back to back without waiting (their addresses follow from the deployer's nonces), so the
// whole setup takes three or four blocks rather than ten.
func Setup(ctx context.Context, d *client.Client, p SetupParams) (*Market, error) {
	m := &Market{}
	var txs []*types.Transaction
	deploy := func(con client.Contract, args ...any) (common.Address, error) {
		data, err := con.DeployData(args...)
		if err != nil {
			return common.Address{}, err
		}
		tx, err := d.Send(ctx, client.Tx{Data: data})
		if err != nil {
			return common.Address{}, fmt.Errorf("deploy %s: %w", con.Name, err)
		}
		txs = append(txs, tx)
		return crypto.CreateAddress(d.Account, tx.Nonce()), nil
	}
	var err error
	if m.USD, err = deploy(client.TokenContract, "US Dollar", "USD"); err != nil {
		return nil, err
	}
	if m.WETH, err = deploy(client.TokenContract, "Wrapped Ether", "WETH"); err != nil {
		return nil, err
	}
	if m.Pool, err = deploy(client.AMMContract, m.WETH, m.USD); err != nil {
		return nil, err
	}
	if m.Feed, err = deploy(client.FeedOracleContract, E18(p.Price)); err != nil {
		return nil, err
	}
	if m.Spot, err = deploy(client.AMMOracleContract, m.Pool); err != nil {
		return nil, err
	}
	m.LendingOracle = m.Feed
	if p.UseSpotOracle {
		m.LendingOracle = m.Spot
	}
	rate := p.InterestPerS
	if rate == nil {
		rate = new(big.Int)
	}
	if m.Lending, err = deploy(client.LendingContract, m.WETH, m.USD, m.LendingOracle, rate); err != nil {
		return nil, err
	}
	for _, tx := range txs {
		if _, err := d.Wait(ctx, tx); err != nil {
			return nil, fmt.Errorf("deploy: %w", err)
		}
	}

	// Seed pool liquidity and lending liquidity from the deployer.
	poolUSD := p.PoolWETH * p.Price
	if err := Batch(ctx, d, []Call{
		{client.TokenContract, m.WETH, "mint", []any{d.Account, E18(p.PoolWETH)}},
		{client.TokenContract, m.USD, "mint", []any{d.Account, E18(poolUSD + p.LendingUSD)}},
		{client.TokenContract, m.WETH, "approve", []any{m.Pool, MaxUint}},
		{client.TokenContract, m.USD, "approve", []any{m.Pool, MaxUint}},
	}); err != nil {
		return nil, err
	}
	if err := Batch(ctx, d, []Call{
		{client.AMMContract, m.Pool, "addLiquidity", []any{E18(p.PoolWETH), E18(poolUSD)}},
		{client.TokenContract, m.USD, "transfer", []any{m.Lending, E18(p.LendingUSD)}},
	}); err != nil {
		return nil, err
	}
	return m, nil
}

// Call is one contract call in a Batch.
type Call struct {
	Con    client.Contract
	Addr   common.Address
	Method string
	Args   []any
}

// Batch submits independent calls from one account concurrently and waits for all of
// them. Use it only when the calls do not depend on each other's order.
func Batch(ctx context.Context, c *client.Client, calls []Call) error {
	errs := make(chan error, len(calls))
	sem := make(chan struct{}, 32)
	for _, call := range calls {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			_, err := c.Transact(ctx, call.Con, call.Addr, call.Method, call.Args...)
			errs <- err
		}()
	}
	var first error
	for range calls {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// FundCalls returns the mints that give an account its starting balances (deployer only).
func (m *Market) FundCalls(to common.Address, usd, weth float64) []Call {
	var calls []Call
	if usd > 0 {
		calls = append(calls, Call{client.TokenContract, m.USD, "mint", []any{to, E18(usd)}})
	}
	if weth > 0 {
		calls = append(calls, Call{client.TokenContract, m.WETH, "mint", []any{to, E18(weth)}})
	}
	return calls
}

// ApproveAll lets the pool and the lending market pull both tokens from c's account.
func (m *Market) ApproveAll(ctx context.Context, c *client.Client) error {
	var calls []Call
	for _, tok := range []common.Address{m.USD, m.WETH} {
		for _, spender := range []common.Address{m.Pool, m.Lending} {
			calls = append(calls, Call{client.TokenContract, tok, "approve", []any{spender, MaxUint}})
		}
	}
	return Batch(ctx, c, calls)
}

// Snapshot is one consistent-enough read of the market (several view calls, so it can
// straddle a block boundary; agents must tolerate that, as on a real chain).
type Snapshot struct {
	Block       uint64  `json:"block"`
	ReserveWETH float64 `json:"reserveWETH"`
	ReserveUSD  float64 `json:"reserveUSD"`
	PoolPrice   float64 `json:"poolPrice"`
	FeedPrice   float64 `json:"feedPrice"`
	// LendPrice is what the lending market values collateral at: the feed, or the AMM
	// spot price when it was deployed with the spot oracle.
	LendPrice float64 `json:"lendPrice"`
	// Raw reserves for exact AMM maths.
	RawWETH, RawUSD *big.Int `json:"-"`
}

// Read takes a market snapshot.
func (m *Market) Read(ctx context.Context, c *client.Client) (*Snapshot, error) {
	r0, err := c.ViewBig(ctx, client.AMMContract, m.Pool, "reserve0")
	if err != nil {
		return nil, err
	}
	r1, err := c.ViewBig(ctx, client.AMMContract, m.Pool, "reserve1")
	if err != nil {
		return nil, err
	}
	feed, err := c.ViewBig(ctx, client.FeedOracleContract, m.Feed, "price")
	if err != nil {
		return nil, err
	}
	height, _ := c.BlockNumber(ctx)
	s := &Snapshot{Block: height, RawWETH: r0, RawUSD: r1, ReserveWETH: F(r0), ReserveUSD: F(r1), FeedPrice: F(feed)}
	if s.ReserveWETH > 0 {
		s.PoolPrice = s.ReserveUSD / s.ReserveWETH
	}
	s.LendPrice = s.FeedPrice
	if m.LendingOracle != m.Feed {
		lp, err := c.ViewBig(ctx, client.AMMOracleContract, m.LendingOracle, "price")
		if err != nil {
			return nil, err
		}
		s.LendPrice = F(lp)
	}
	return s, nil
}

// AmountOut mirrors AMM.getAmountOut exactly (integer maths, 0.3% fee).
func AmountOut(amountIn, reserveIn, reserveOut *big.Int) *big.Int {
	inWithFee := new(big.Int).Mul(amountIn, big.NewInt(9970))
	num := new(big.Int).Mul(inWithFee, reserveOut)
	den := new(big.Int).Add(new(big.Int).Mul(reserveIn, big.NewInt(10000)), inWithFee)
	return num.Div(num, den)
}

// ArbAmountIn is the input that moves the pool price to `target` (USD per WETH), ignoring
// the fee: for buying WETH with USD, dy = sqrt(k*target) - y; for selling WETH, dx =
// sqrt(k/target) - x. Returns the side and amount; zero if already at the target.
func ArbAmountIn(s *Snapshot, target float64) (buyWETH bool, amountIn float64) {
	k := s.ReserveWETH * s.ReserveUSD
	if target > s.PoolPrice {
		return true, math.Max(0, math.Sqrt(k*target)-s.ReserveUSD)
	}
	return false, math.Max(0, math.Sqrt(k/target)-s.ReserveWETH)
}

// Holdings is an account's token balances and lending position.
type Holdings struct {
	USD        float64 `json:"usd"`
	WETH       float64 `json:"weth"`
	Collateral float64 `json:"collateral"`
	Debt       float64 `json:"debt"`
	Health     float64 `json:"health"` // +Inf with no debt

	// The exact on-chain amounts (18 decimals), for CapTo.
	RawUSD, RawWETH, RawCollateral *big.Int `json:"-"`
}

// CapTo converts a float amount for a transaction to 18 decimals, or returns exactly the
// on-chain balance if the amount is within 1e-6 of it: a float read of a balance
// converted back with E18 is off by up to 0.5e-6, so "deposit all my WETH" would revert
// half the time (a little above the balance) or leave dust (below). Amounts clearly above
// the balance are kept, so that the transaction reverts.
func CapTo(amount float64, balance *big.Int) *big.Int {
	x := E18(amount)
	if balance != nil && new(big.Int).Abs(new(big.Int).Sub(x, balance)).Cmp(big.NewInt(1e12)) <= 0 {
		return new(big.Int).Set(balance)
	}
	return x
}

// NetWorth values the holdings in USD at the given price.
func (h Holdings) NetWorth(price float64) float64 {
	return h.USD + (h.WETH+h.Collateral)*price - h.Debt
}

// Holdings reads an account's position.
func (m *Market) Holdings(ctx context.Context, c *client.Client, who common.Address) (Holdings, error) {
	var h Holdings
	var rawDebt *big.Int
	reads := []struct {
		con    client.Contract
		addr   common.Address
		method string
		dst    *float64
		raw    **big.Int
	}{
		{client.TokenContract, m.USD, "balanceOf", &h.USD, &h.RawUSD},
		{client.TokenContract, m.WETH, "balanceOf", &h.WETH, &h.RawWETH},
		{client.LendingContract, m.Lending, "collateralOf", &h.Collateral, &h.RawCollateral},
		{client.LendingContract, m.Lending, "debtOf", &h.Debt, &rawDebt},
	}
	for _, r := range reads {
		v, err := c.ViewBig(ctx, r.con, r.addr, r.method, who)
		if err != nil {
			return h, err
		}
		*r.dst, *r.raw = F(v), v
	}
	hf, err := c.ViewBig(ctx, client.LendingContract, m.Lending, "healthFactor", who)
	if err != nil {
		return h, err
	}
	if hf.Cmp(MaxUint) == 0 {
		h.Health = math.Inf(1)
	} else {
		h.Health = F(hf)
	}
	return h, nil
}

func (h Holdings) String() string {
	hf := "∞"
	if !math.IsInf(h.Health, 1) {
		hf = fmt.Sprintf("%.3f", h.Health)
	}
	return fmt.Sprintf("USD %.0f, WETH %.3f, collateral %.3f, debt %.0f, health %s", h.USD, h.WETH, h.Collateral, h.Debt, hf)
}
