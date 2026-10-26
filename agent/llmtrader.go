package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"defichain/client"
)

// LLMTrader asks a language model what to do each round. It sees the market, its own
// position and its recent history, and answers with a short plan of actions. Every action
// is validated and clamped in Go before it touches the chain: the model proposes, code
// disposes.
type LLMTrader struct {
	*Base
	LLM      *LLM
	Every    time.Duration
	Persona  string // short name of the persona, recorded with every decision
	Strategy string // free-text persona appended to the system prompt
	// Truth, if set, is shown to the model as the exchange price (what a real trader sees
	// on Binance); the on-chain feed can lag it.
	Truth *Truth
	// Tape, if set, shows the model what everyone else traded recently.
	Tape *Tape

	history  []string
	prices   []float64
	latency  time.Duration // of the last LLM call
	lastColl float64       // collateral and debt after our own last actions, to notice
	lastDebt float64       // liquidations that happened in between
	seen     bool
}

// Action is one step of a plan.
type Action struct {
	Action    string  `json:"action"` // buy_weth | sell_weth | deposit | withdraw | borrow | repay
	AmountUSD float64 `json:"amount_usd,omitempty"`
	AmountETH float64 `json:"amount_weth,omitempty"`
}

// Decision is the JSON the model returns: up to MaxActions actions, executed in order.
// The single-action form {"action": ...} is accepted too.
type Decision struct {
	Actions []Action `json:"actions"`
	Action
	Reason string `json:"reason"`
}

// MaxActions bounds a plan, so one decision cannot do unlimited damage.
const MaxActions = 3

// Slippage is the tolerance on swaps: the swap reverts if the pool moved against the quote
// by more than this before inclusion (the same 3% as the rule-based Deleverager).
const Slippage = 0.03

func (d Decision) plan() []Action {
	plan := d.Actions
	if len(plan) == 0 && d.Action.Action != "" && d.Action.Action != "hold" {
		plan = []Action{d.Action}
	}
	var out []Action
	for _, a := range plan {
		if a.Action != "" && a.Action != "hold" {
			out = append(out, a)
		}
	}
	if len(out) > MaxActions {
		out = out[:MaxActions]
	}
	return out
}

// direction is the plan's intended change of WETH exposure: +1 buys, -1 sells, 0 neither
// (or both). It is what herding measures compare across traders.
func direction(plan []Action) int {
	buy, sell := false, false
	for _, a := range plan {
		switch a.Action {
		case "buy_weth":
			buy = true
		case "sell_weth":
			sell = true
		}
	}
	switch {
	case buy && !sell:
		return 1
	case sell && !buy:
		return -1
	}
	return 0
}

const llmSystem = `You are an autonomous trading agent on a small DeFi chain with two tokens, USD and WETH.
Venues:
- AMM pool (constant product x*y=k, 0.3% fee). Swapping moves the pool price against you; large trades have more price impact.
- Lending market: deposit WETH as collateral, borrow USD up to 75% of the collateral's value (valued at the oracle feed price).
  A position whose debt exceeds 80% of its collateral value (health factor < 1.0) is liquidated: a liquidator repays up to
  half of the debt and seizes collateral worth that amount plus a 5% bonus. Debt accrues a small interest.
The exchange price is the real market price (a centralised exchange you cannot trade on here). The on-chain oracle feed
follows it with a short delay, and the lending market uses the feed. The pool price is moved by trades and arbitrage.
Your goal: maximise your net worth in USD (USD + WETH*price + collateral*price - debt), valued at the exchange price.
You decide every few seconds; the market keeps moving in between.
Reply with a single JSON object:
{"actions": [{"action": "buy_weth"|"sell_weth"|"deposit"|"withdraw"|"borrow"|"repay",
              "amount_usd": <number, for buy_weth/borrow/repay>,
              "amount_weth": <number, for sell_weth/deposit/withdraw>}, ...],
 "reason": "<one sentence>"}
At most 3 actions, executed in order as separate transactions. An empty list means hold.
Example, adding leverage: {"actions": [{"action": "deposit", "amount_weth": 20}, {"action": "borrow", "amount_usd": 25000},
{"action": "buy_weth", "amount_usd": 25000}], "reason": "..."}`

func (t *LLMTrader) Run(ctx context.Context) {
	for sleep(ctx, t.Every) {
		s, err := t.M.Read(ctx, t.C)
		if err != nil {
			continue
		}
		h, err := t.M.Holdings(ctx, t.C, t.Account())
		if err != nil {
			continue
		}
		ref := s.FeedPrice
		if t.Truth != nil {
			ref = t.Truth.Price()
		}
		t.prices = append(t.prices, ref)
		if len(t.prices) > 12 {
			t.prices = t.prices[1:]
		}
		alert := t.noticeLiquidation(s, h)

		var d Decision
		callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		asked := time.Now()
		_, err = t.LLM.ChatJSON(callCtx, llmSystem+"\n"+t.Strategy, t.prompt(s, h, ref, alert), &d)
		cancel()
		t.latency = time.Since(asked)
		if err != nil {
			if ctx.Err() == nil {
				t.emit("llm-error", truncate(err.Error(), 160), 0)
			}
			continue
		}
		plan := d.plan()
		dir := direction(plan)
		t.emitData("llm-decision", fmt.Sprintf("[%s] %s | %s", t.latency.Round(100*time.Millisecond), describe(plan), truncate(d.Reason, 200)),
			float64(dir), map[string]float64{"dir": float64(dir), "actions": float64(len(plan)), "latency": t.latency.Seconds()})
		var results []string
		if len(plan) == 0 {
			t.emit("llm-hold", fmt.Sprintf("[%s] %s", t.latency.Round(100*time.Millisecond), truncate(d.Reason, 150)), 0)
			results = append(results, "held")
		}
		for _, a := range plan {
			res, ok := t.act(ctx, a, d.Reason)
			results = append(results, res)
			if !ok {
				break // later steps usually depend on this one
			}
		}
		t.history = append(t.history, fmt.Sprintf("block %d: %s -> %s", s.Block, describe(plan), strings.Join(results, "; ")))
		if len(t.history) > 8 {
			t.history = t.history[1:]
		}
		if after, err := t.M.Holdings(ctx, t.C, t.Account()); err == nil {
			t.lastColl, t.lastDebt, t.seen = after.Collateral, after.Debt, true
		}
	}
}

// noticeLiquidation compares the position with what our own last actions left. Only a
// liquidator can take collateral away in between, so a drop means we were liquidated.
func (t *LLMTrader) noticeLiquidation(s *Snapshot, h Holdings) string {
	if !t.seen || h.Collateral >= t.lastColl-1e-9 {
		return ""
	}
	seized, repaid := t.lastColl-h.Collateral, t.lastDebt-h.Debt
	t.emitData("llm-was-liquidated", fmt.Sprintf("lost %.3f WETH collateral, %.0f USD debt repaid", seized, repaid), seized,
		map[string]float64{"seized": seized, "repaid": repaid})
	t.history = append(t.history, fmt.Sprintf("block %d: LIQUIDATED: %.3f WETH of collateral seized, %.0f USD of debt repaid by a liquidator", s.Block, seized, repaid))
	return fmt.Sprintf("ALERT: since your last decision your position was LIQUIDATED: %.3f WETH of your collateral was seized and %.0f USD of your debt was repaid by a liquidator (you lost the 5%% bonus on that).",
		seized, repaid)
}

func (t *LLMTrader) prompt(s *Snapshot, h Holdings, ref float64, alert string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Block %d.\n", s.Block)
	if t.Truth != nil {
		fmt.Fprintf(&b, "Exchange price: %.2f USD/WETH. ", ref)
	}
	fmt.Fprintf(&b, "Oracle feed: %.2f. Pool price: %.2f (pool vs exchange %+.2f%%). Pool depth: %.1f WETH / %.0f USD.\n",
		s.FeedPrice, s.PoolPrice, (s.PoolPrice-ref)/ref*100, s.ReserveWETH, s.ReserveUSD)
	fmt.Fprintf(&b, "Recent exchange prices, one per decision, oldest first: %s\n", joinPrices(t.prices))
	if t.Tape != nil {
		b.WriteString(t.Tape.Summary(time.Since(t.T0).Seconds(), 30, t.ID) + "\n")
	}
	fmt.Fprintf(&b, "Your position: %s. Net worth: %.0f USD.\n", h, h.NetWorth(ref))
	if h.Debt > 0 {
		fmt.Fprintf(&b, "Your position is liquidated if the lending market's price (now %.2f) falls below %.2f.\n",
			s.LendPrice, h.Debt/(0.8*h.Collateral))
	}
	if room := h.Collateral*s.LendPrice*0.75 - h.Debt; room > 0 {
		fmt.Fprintf(&b, "You can borrow up to %.0f more USD against your current collateral.\n", room)
	}
	if alert != "" {
		b.WriteString(alert + "\n")
	}
	if len(t.history) > 0 {
		fmt.Fprintf(&b, "Your recent decisions:\n- %s\n", strings.Join(t.history, "\n- "))
	}
	b.WriteString("What do you do now?")
	return b.String()
}

// act validates one action against fresh balances, clamps it, and sends at most one
// transaction. It reports a short result and whether the plan may continue.
func (t *LLMTrader) act(ctx context.Context, a Action, reason string) (string, bool) {
	// The model may take seconds to answer and the pool moves meanwhile: quote swaps
	// against fresh reserves, or the slippage limit is computed from a stale price.
	s, err := t.M.Read(ctx, t.C)
	if err != nil {
		return "skipped: read failed", false
	}
	h, err := t.M.Holdings(ctx, t.C, t.Account())
	if err != nil {
		return "skipped: read failed", false
	}
	finite := func(x float64) float64 {
		if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
			return 0
		}
		return x
	}
	usd, weth := finite(a.AmountUSD), finite(a.AmountETH)
	var detail string
	swap := false
	switch a.Action {
	case "buy_weth":
		usd = math.Min(usd, h.USD*0.99)
		if usd <= 0 {
			return "rejected: no USD", false
		}
		min := F(AmountOut(E18(usd), s.RawUSD, s.RawWETH)) * (1 - Slippage)
		_, err = t.C.Transact(ctx, client.AMMContract, t.M.Pool, "swap", t.M.USD, CapTo(usd, h.RawUSD), E18(min))
		detail, swap = fmt.Sprintf("buy WETH with %.0f USD", usd), true
	case "sell_weth":
		weth = math.Min(weth, h.WETH*0.99)
		if weth <= 0 {
			return "rejected: no WETH in wallet", false
		}
		min := F(AmountOut(E18(weth), s.RawWETH, s.RawUSD)) * (1 - Slippage)
		_, err = t.C.Transact(ctx, client.AMMContract, t.M.Pool, "swap", t.M.WETH, CapTo(weth, h.RawWETH), E18(min))
		detail, swap = fmt.Sprintf("sell %.3f WETH", weth), true
	case "deposit":
		weth = math.Min(weth, h.WETH)
		if weth <= 0 {
			return "rejected: no WETH in wallet", false
		}
		_, err = t.C.Transact(ctx, client.LendingContract, t.M.Lending, "deposit", CapTo(weth, h.RawWETH))
		detail = fmt.Sprintf("deposit %.3f WETH", weth)
	case "withdraw":
		weth = math.Min(weth, h.Collateral)
		if weth <= 0 {
			return "rejected: no collateral", false
		}
		_, err = t.C.Transact(ctx, client.LendingContract, t.M.Lending, "withdraw", CapTo(weth, h.RawCollateral))
		detail = fmt.Sprintf("withdraw %.3f WETH", weth)
	case "borrow":
		// Clamp to the room at the oracle price now: the model computed its amount from a
		// snapshot taken seconds ago, and in a falling market the limit shrinks meanwhile.
		room := h.Collateral*s.LendPrice*0.75 - h.Debt
		usd = math.Min(usd, room*0.99)
		if usd <= 0 {
			return "rejected: no borrowing room (deposit collateral first)", false
		}
		_, err = t.C.Transact(ctx, client.LendingContract, t.M.Lending, "borrow", E18(usd))
		detail = fmt.Sprintf("borrow %.0f USD", usd)
	case "repay":
		usd = math.Min(math.Min(usd, h.USD), h.Debt*1.001)
		if usd <= 0 {
			return "rejected: nothing to repay", false
		}
		_, err = t.C.Transact(ctx, client.LendingContract, t.M.Lending, "repay", CapTo(usd, h.RawUSD))
		detail = fmt.Sprintf("repay %.0f USD", usd)
	default:
		t.emit("llm-invalid", fmt.Sprintf("unknown action %q", a.Action), 0)
		return "rejected: unknown action " + a.Action, false
	}
	if err != nil {
		t.emit("llm-reverted", fmt.Sprintf("%s: %v", detail, err), 0)
		return "reverted: " + truncate(err.Error(), 80), false
	}
	after, _ := t.M.Holdings(ctx, t.C, t.Account())
	ref := s.FeedPrice
	if t.Truth != nil {
		ref = t.Truth.Price()
	}
	// Swaps carry "weth" (the tape and the flow analysis read it); lending actions do not.
	data := map[string]float64{"usd": after.USD - h.USD, "coll": after.Collateral - h.Collateral, "debt": after.Debt - h.Debt}
	if swap {
		data["weth"] = after.WETH - h.WETH
	} else {
		data["walletWeth"] = after.WETH - h.WETH
	}
	t.emitData("llm-"+a.Action, fmt.Sprintf("%s (%s) | net worth %.0f", detail, truncate(reason, 120), after.NetWorth(ref)), after.NetWorth(ref), data)
	return "ok: " + detail, true
}

func describe(plan []Action) string {
	if len(plan) == 0 {
		return "hold"
	}
	b, _ := json.Marshal(plan)
	return string(b)
}

func joinPrices(p []float64) string {
	parts := make([]string, len(p))
	for i, x := range p {
		parts[i] = fmt.Sprintf("%.0f", x)
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
