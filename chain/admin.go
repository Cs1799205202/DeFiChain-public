package chain

// Framework file: GET /status and the admin API (/admin/...), for you, the devnet tool,
// the simulator and the tests. All bodies are JSON. Requests from web pages are refused:
// the API has no authentication and must only be used from this machine.
//
//	GET  /status                     spec.Status
//	POST /admin/mine      {"blocks":n}       mine n blocks now (auto-mining must be off)
//	GET  /admin/miner                        spec.MinerState
//	POST /admin/miner     {"mining":true,"hashrate":100000}
//	GET  /admin/net                          spec.NetConditions
//	POST /admin/net       {"blocked":["n2"],"delay":"200ms","jitter":"100ms","drop":0.1}
//	POST /admin/peers     {"add":{"n5":"http://127.0.0.1:28005"},"remove":["n4"]}
//	GET  /admin/reorgs                       spec.Reorgs
//	GET  /admin/chain?from=&to=              canonical blocks, []spec.BlockInfo
//	GET  /admin/blocks?from=                 every stored block (the block tree)

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"defichain/spec"
)

func (n *Node) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", n.handleStatus)
	mux.HandleFunc("POST /admin/mine", n.adminOnly(n.handleMine))
	mux.HandleFunc("GET /admin/miner", n.handleMiner)
	mux.HandleFunc("POST /admin/miner", n.adminOnly(n.handleMiner))
	mux.HandleFunc("GET /admin/net", n.handleNet)
	mux.HandleFunc("POST /admin/net", n.adminOnly(n.handleNet))
	mux.HandleFunc("POST /admin/peers", n.adminOnly(n.handlePeers))
	mux.HandleFunc("GET /admin/reorgs", n.handleReorgs)
	mux.HandleFunc("GET /admin/chain", n.handleChain)
	mux.HandleFunc("GET /admin/blocks", n.handleBlocks)
	mux.HandleFunc("GET /{$}", n.handleHelp)
}

// adminOnly refuses state-changing requests sent by a browser page (which always carries
// an Origin header); curl, Go and Python clients do not send one.
func (n *Node) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			writeErr(w, http.StatusForbidden, "admin API refuses requests from web pages")
			return
		}
		h(w, r)
	}
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// readJSON decodes an optional JSON body into v.
func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(data) == 0 {
		return err
	}
	return json.Unmarshal(data, v)
}

func (n *Node) ref(b *types.Block) spec.BlockRef {
	return spec.BlockRef{Number: b.NumberU64(), Hash: b.Hash(), TD: spec.NewBig(n.Chain.GetTd(b.Hash()))}
}

// Status is this node's GET /status.
func (n *Node) Status() spec.Status {
	_, count := n.Chain.Reorgs()
	return spec.Status{
		ID: n.cfg.ID, Genesis: n.Chain.Genesis().Hash(), Head: n.ref(n.Chain.CurrentBlock()), Peers: n.Net.Peers(),
		Mining: n.Miner.Mining(), Hashrate: n.Throttle.Rate(), Coinbase: n.Coinbase.Hex(), TxPool: n.Pool.Len(),
		Reorgs: count, Blocks: n.Chain.NumBlocks(), Net: n.Net.Conditions(), Uptime: time.Since(n.started).Seconds(),
		PID: os.Getpid(),
	}
}

func (n *Node) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, n.Status())
}

func (n *Node) handleMine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Blocks int `json:"blocks"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if q := r.URL.Query().Get("blocks"); q != "" {
		req.Blocks, _ = strconv.Atoi(q)
	}
	if req.Blocks <= 0 {
		req.Blocks = 1
	}
	if req.Blocks > 1000 {
		writeErr(w, http.StatusBadRequest, "at most 1000 blocks per request")
		return
	}
	if n.Miner.Mining() {
		writeErr(w, http.StatusConflict, "auto-mining is on; stop it first (POST /admin/miner {\"mining\":false})")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	var res spec.MineResult
	for i := 0; i < req.Blocks; i++ {
		b, err := n.Miner.MineBlock(ctx)
		if err != nil {
			res.Error = err.Error()
			writeJSON(w, http.StatusInternalServerError, res)
			return
		}
		res.Blocks = append(res.Blocks, n.ref(b))
	}
	writeJSON(w, http.StatusOK, res)
}

func (n *Node) minerState() spec.MinerState {
	mining, rate := n.Miner.Mining(), n.Throttle.Rate()
	return spec.MinerState{Mining: &mining, Hashrate: &rate}
}

func (n *Node) handleMiner(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req spec.MinerState
		if err := readJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		if req.Hashrate != nil {
			n.Throttle.SetRate(*req.Hashrate)
			n.log.Printf("hashrate set to %s", fmtRate(*req.Hashrate))
		}
		if req.Mining != nil {
			if *req.Mining {
				n.Miner.Start()
			} else {
				n.Miner.Stop()
			}
		}
	}
	writeJSON(w, http.StatusOK, n.minerState())
}

func (n *Node) handleNet(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var c spec.NetConditions
		if err := readJSON(r, &c); err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		if c.Drop < 0 || c.Drop > 1 || c.Delay < 0 || c.Jitter < 0 {
			writeErr(w, http.StatusBadRequest, "drop must be in [0,1], delay and jitter >= 0")
			return
		}
		n.Net.SetConditions(c)
		n.log.Printf("net conditions: blocked=%v delay=%s jitter=%s drop=%.2f", c.Blocked, c.Delay.Std(), c.Jitter.Std(), c.Drop)
	}
	writeJSON(w, http.StatusOK, n.Net.Conditions())
}

func (n *Node) handlePeers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Add    map[string]string `json:"add"`
		Remove []string          `json:"remove"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	for id, url := range req.Add {
		n.Net.AddPeer(id, url)
	}
	for _, id := range req.Remove {
		n.Net.RemovePeer(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": n.Net.Peers()})
}

func (n *Node) handleReorgs(w http.ResponseWriter, r *http.Request) {
	recent, count := n.Chain.Reorgs()
	if recent == nil {
		recent = []spec.ReorgEvent{}
	}
	writeJSON(w, http.StatusOK, spec.Reorgs{Count: count, Recent: recent})
}

func (n *Node) info(b *types.Block) spec.BlockInfo {
	return spec.BlockInfo{Number: b.NumberU64(), Hash: b.Hash(), Parent: b.ParentHash(), Miner: b.Coinbase(), Time: b.Time(),
		Difficulty: spec.NewBig(b.Difficulty()), TD: spec.NewBig(n.Chain.GetTd(b.Hash())), Txs: len(b.Transactions()),
		GasUsed: b.GasUsed(), Canonical: n.Chain.IsCanonical(b.Hash())}
}

func (n *Node) handleChain(w http.ResponseWriter, r *http.Request) {
	head := n.Chain.CurrentBlock().NumberU64()
	to, err := strconv.ParseUint(r.URL.Query().Get("to"), 10, 64)
	if err != nil || to > head {
		to = head
	}
	from, err := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	if err != nil {
		from = 0
		if to > 49 {
			from = to - 49
		}
	}
	if to >= from && to-from > 5000 {
		from = to - 5000
	}
	out := []spec.BlockInfo{}
	for i := from; i <= to; i++ {
		if b := n.Chain.GetBlockByNumber(i); b != nil {
			out = append(out, n.info(b))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (n *Node) handleBlocks(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	out := []spec.BlockInfo{}
	for _, b := range n.Chain.AllBlocks() {
		if b.NumberU64() >= from {
			out = append(out, n.info(b))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Number != out[j].Number {
			return out[i].Number < out[j].Number
		}
		return out[i].Hash.Hex() < out[j].Hash.Hex()
	})
	writeJSON(w, http.StatusOK, out)
}

func (n *Node) handleHelp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "defichain node %s\n\nJSON-RPC: POST / or /rpc (eth_*, net_*, web3_*, txpool_*)\n"+
		"Status:   GET /status\nAdmin:    /admin/mine /admin/miner /admin/net /admin/peers /admin/reorgs /admin/chain /admin/blocks\n"+
		"P2P:      /p2p/block /p2p/tx /p2p/head /p2p/ancestors\n", n.cfg.ID)
}
