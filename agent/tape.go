package agent

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Tape is the public record of recent pool trades and liquidations, what anyone can read
// from a block explorer. LLM traders that are given it see what everyone else is doing,
// which is how herding by information cascade can start.
type Tape struct {
	mu      sync.Mutex
	entries []tapeEntry
}

type tapeEntry struct {
	t      float64
	agent  string
	kind   string
	weth   float64 // signed: + bought from the pool, - sold into it
	seized float64 // liquidations: collateral seized
}

// Add records an event if it is a pool trade or a liquidation.
func (tp *Tape) Add(e Event) {
	weth, trade := e.Data["weth"]
	if (!trade || weth == 0) && e.Kind != "liquidate" {
		return
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.entries = append(tp.entries, tapeEntry{t: e.T, agent: e.Agent, kind: e.Kind, weth: weth, seized: e.Data["seized"]})
	// Keep the last few minutes.
	for len(tp.entries) > 0 && tp.entries[0].t < e.T-300 {
		tp.entries = tp.entries[1:]
	}
}

// Summary describes the activity of everyone but self in (now-window, now].
func (tp *Tape) Summary(now, window float64, self string) string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var buys, sells, seized float64
	var trades, liqs int
	var big []tapeEntry
	for _, e := range tp.entries {
		if e.t <= now-window || e.t > now || e.agent == self {
			continue
		}
		if e.kind == "liquidate" {
			liqs++
			seized += e.seized
			continue
		}
		trades++
		if e.weth > 0 {
			buys += e.weth
		} else {
			sells -= e.weth
		}
		big = append(big, e)
	}
	if trades == 0 && liqs == 0 {
		return fmt.Sprintf("Recent on-chain activity (last %.0f s): no trades by others.", window)
	}
	var b strings.Builder
	net := buys - sells
	side := "bought from"
	if net < 0 {
		side = "sold into"
	}
	fmt.Fprintf(&b, "Recent on-chain activity by others (last %.0f s): %d pool trades, net %.1f WETH %s the pool (bought %.1f, sold %.1f).",
		window, trades, math.Abs(net), side, buys, sells)
	sort.Slice(big, func(i, j int) bool { return math.Abs(big[i].weth) > math.Abs(big[j].weth) })
	if len(big) > 3 {
		big = big[:3]
	}
	if len(big) > 0 {
		var parts []string
		for _, e := range big {
			what := "bought"
			if e.weth < 0 {
				what = "sold"
			}
			label := ""
			if e.kind == "dump" {
				label = " (a liquidator selling seized collateral)"
			}
			parts = append(parts, fmt.Sprintf("%s %.1f WETH%s", what, math.Abs(e.weth), label))
		}
		fmt.Fprintf(&b, " Largest: %s.", strings.Join(parts, "; "))
	}
	if liqs > 0 {
		fmt.Fprintf(&b, " Liquidations: %d (%.1f WETH of collateral seized).", liqs, seized)
	}
	return b.String()
}
