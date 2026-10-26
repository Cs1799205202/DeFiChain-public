package main

// External agents: your own trading programs, in any language, run as subprocesses. Each
// gets a funded account, the market's contract addresses and ABIs (world.json), and the
// exchange API (the "real world" price, and an account to hedge on). See docs/AGENTS.md.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/agent"
	"defichain/client"
)

type external struct {
	name    string
	command string
	addr    common.Address
	token   string // authenticates the agent to the exchange API
	logPath string
	decPath string // the agent's decision log (JSON lines), optional
}

// parseExternal reads -agent "name=command args...".
func parseExternal(specs []string, builtin []string) ([]*external, error) {
	var out []*external
	seen := map[string]bool{}
	for _, b := range builtin {
		seen[b] = true // a built-in agent's name would give us the same account (same key)
	}
	for _, s := range specs {
		name, cmd, ok := strings.Cut(s, "=")
		name, cmd = strings.TrimSpace(name), strings.TrimSpace(cmd)
		if !ok || name == "" || cmd == "" {
			return nil, fmt.Errorf(`-agent %q: want "name=command args..."`, s)
		}
		if seen[name] || reserved(name) {
			return nil, fmt.Errorf("-agent: name %q is taken (by a built-in agent or another -agent); its key would be shared", name)
		}
		seen[name] = true
		tok := make([]byte, 12)
		rand.Read(tok) //nolint:errcheck
		out = append(out, &external{name: name, command: cmd, addr: client.AddressFor(name), token: hex.EncodeToString(tok)})
	}
	return out, nil
}

// reserved reports whether name collides with a built-in account.
func reserved(name string) bool {
	switch name {
	case "deployer", "recorder", "sink", "oracle", "sim":
		return true
	}
	return false
}

// world.json: everything an external agent needs to find the market.
type worldFile struct {
	ChainID   uint64                     `json:"chainId"`
	RPC       []string                   `json:"rpc"`
	Exchange  string                     `json:"exchange"`
	Contracts map[string]common.Address  `json:"contracts"`
	ABIs      map[string]json.RawMessage `json:"abis"`
	Params    map[string]any             `json:"params"`
}

func (w *world) writeWorld() (string, error) {
	dir := w.c.Out
	if dir == "" {
		dir = w.net.Dir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	abis := map[string]json.RawMessage{}
	for _, con := range []client.Contract{client.TokenContract, client.AMMContract, client.LendingContract,
		client.FeedOracleContract, client.AMMOracleContract} {
		abis[con.Name] = json.RawMessage(con.ABIJSON)
	}
	m := w.market
	wf := worldFile{
		ChainID: w.net.Genesis.Config.ChainID, RPC: w.net.URLs(), Exchange: fmt.Sprintf("http://127.0.0.1:%d", w.c.ExchangePort),
		Contracts: map[string]common.Address{"USD": m.USD, "WETH": m.WETH, "AMM": m.Pool, "FeedOracle": m.Feed,
			"AMMOracle": m.Spot, "Lending": m.Lending, "LendingOracle": m.LendingOracle},
		ABIs: abis,
		Params: map[string]any{"ltv": 0.75, "liquidationThreshold": 0.80, "liquidationBonus": 0.05, "closeFactor": 0.5,
			"ammFee": 0.003, "durationSeconds": w.c.Duration.Seconds(), "startPrice": w.c.Price},
	}
	path := filepath.Join(dir, "world.json")
	return path, writeJSON(path, wf)
}

// runExternal starts an external agent and stops it when ctx ends.
func (w *world) runExternal(ctx context.Context, e *external, worldPath string) {
	dir := filepath.Dir(worldPath)
	e.logPath = filepath.Join(dir, "agent-"+e.name+".log")
	e.decPath = filepath.Join(dir, "agent-"+e.name+".decisions.jsonl")
	logf, err := os.Create(e.logPath)
	if err != nil {
		w.note("agent-error", fmt.Sprintf("%s: %v", e.name, err))
		return
	}
	defer logf.Close()
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.Command(shell, flag, e.command)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(),
		"DEFICHAIN_RPC="+strings.Join(w.net.URLs(), ","),
		"DEFICHAIN_WORLD="+worldPath,
		"DEFICHAIN_AGENT="+e.name,
		"DEFICHAIN_KEY="+client.KeyHex(client.KeyFor(e.name)),
		"DEFICHAIN_ADDRESS="+e.addr.Hex(),
		fmt.Sprintf("DEFICHAIN_EXCHANGE=http://127.0.0.1:%d", w.c.ExchangePort),
		"DEFICHAIN_EXCHANGE_TOKEN="+e.token,
		fmt.Sprintf("DEFICHAIN_DURATION=%.0f", w.c.Duration.Seconds()),
		"DEFICHAIN_DECISIONS="+e.decPath,
	)
	if err := cmd.Start(); err != nil {
		w.note("agent-error", fmt.Sprintf("%s: start %q: %v", e.name, e.command, err))
		return
	}
	w.note("agent-start", fmt.Sprintf("%s: %s (log %s)", e.name, e.command, e.logPath))
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		w.note("agent-exit", fmt.Sprintf("%s exited early: %v (see %s)", e.name, err, e.logPath))
		return
	case <-ctx.Done():
	}
	if runtime.GOOS == "windows" || cmd.Process.Signal(os.Interrupt) != nil {
		cmd.Process.Kill() //nolint:errcheck
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		<-exited
	}
}

// decisions counts an external agent's decision log: one JSON object per line, with an
// "actions" list (empty = hold); a line with "error": true is a failed decision (e.g. the
// LLM call failed), counted as an error, not as a hold.
func (e *external) decisions() (decisions, trades, holds, errors int) {
	f, err := os.Open(e.decPath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var d struct {
			Actions []json.RawMessage `json:"actions"`
			Error   any               `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &d) != nil {
			continue
		}
		if failed, _ := d.Error.(bool); failed {
			errors++
			continue
		}
		decisions++
		if len(d.Actions) == 0 {
			holds++
		}
		trades += len(d.Actions)
	}
	return
}

// ---- the exchange ----------------------------------------------------------------------

// exchange is the off-chain market (think: a centralised exchange) where the "real" price
// is. External agents can read it and trade on it (to hedge, or to arbitrage against the
// pool). Trading moves its price if -exchange-depth is set.
type exchange struct {
	truth *agent.Truth
	mu    sync.Mutex
	books map[string]*exAccount // agent name -> balances (may go negative: it is a margin account)
	auth  map[string]string     // token -> agent name
}

type exAccount struct {
	USD  float64 `json:"usd"`
	WETH float64 `json:"weth"`
}

func newExchange(t *agent.Truth) *exchange {
	return &exchange{truth: t, books: map[string]*exAccount{}, auth: map[string]string{}}
}

func (x *exchange) value(name string, price float64) float64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	if a := x.books[name]; a != nil {
		return a.USD + a.WETH*price
	}
	return 0
}

func (x *exchange) serve(port int) (func(), error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /price", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]float64{"price": x.truth.Price(), "time": float64(time.Now().UnixMilli()) / 1000})
	})
	mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		name, ok := x.who(r)
		if !ok {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "bad token"})
			return
		}
		x.mu.Lock()
		a := *x.account(name)
		x.mu.Unlock()
		reply(w, http.StatusOK, a)
	})
	mux.HandleFunc("POST /trade", func(w http.ResponseWriter, r *http.Request) {
		name, ok := x.who(r)
		if !ok {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "bad token"})
			return
		}
		var req struct {
			WETH float64 `json:"weth"` // > 0 buy, < 0 sell
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || math.IsNaN(req.WETH) || math.Abs(req.WETH) > 10_000 {
			reply(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"weth\": q}, |q| <= 10000"})
			return
		}
		px := x.truth.Trade(req.WETH)
		x.mu.Lock()
		a := x.account(name)
		a.WETH += req.WETH
		a.USD -= req.WETH * px
		out := map[string]any{"price": px, "account": *a}
		x.mu.Unlock()
		reply(w, http.StatusOK, out)
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("exchange API: %w", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	return func() { srv.Close() }, nil
}

func (x *exchange) account(name string) *exAccount {
	a := x.books[name]
	if a == nil {
		a = &exAccount{}
		x.books[name] = a
	}
	return a
}

func (x *exchange) who(r *http.Request) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	name, ok := x.auth[r.Header.Get("Authorization")]
	return name, ok
}

func (x *exchange) register(e *external) {
	x.mu.Lock()
	x.auth["Bearer "+e.token] = e.name
	x.mu.Unlock()
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
