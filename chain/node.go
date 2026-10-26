package chain

// Framework file: assembles one node and serves its HTTP endpoints.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"defichain/spec"
)

// Node is one full node: block chain, txpool, p2p network, sync handler and miner, plus the
// JSON-RPC server (for wallets and agents) and the admin API (for you and the tests).
type Node struct {
	cfg      spec.NodeConfig
	Chain    *BlockChain
	Pool     *TxPool
	Net      *Network
	Sync     *Handler
	Miner    *Miner
	Throttle *Throttle
	Coinbase common.Address

	log     *log.Logger
	started time.Time
	ln      net.Listener
	srv     *http.Server
	filters *filterSet
}

// NewNode builds a node from its config and the network's genesis.
func NewNode(cfg spec.NodeConfig, g *spec.Genesis, logger *log.Logger) (*Node, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "["+cfg.ID+"] ", log.Ltime|log.Lmicroseconds)
	}
	pool := NewTxPool()
	bc, err := NewBlockChain(g, pool, logger)
	if err != nil {
		return nil, fmt.Errorf("genesis: %w", err)
	}
	n := &Node{cfg: cfg, Chain: bc, Pool: pool, log: logger, filters: newFilterSet()}
	if cfg.Coinbase == "" {
		logger.Printf("warning: no coinbase configured, block rewards go to the zero address")
	}
	n.Coinbase = common.HexToAddress(cfg.Coinbase)
	n.Net = NewNetwork(cfg.ID, cfg.Peers, bc, logger)
	n.Net.verbose = cfg.Verbose
	n.Net.SetConditions(cfg.Net)
	n.Throttle = NewThrottle(cfg.Hashrate)
	n.Sync = NewHandler(bc, n.Net, logger)
	n.Miner = NewMiner(bc, pool, n.Net, n.Coinbase, n.Throttle, logger)
	n.Net.SetHandlers(n.Sync.HandleBlock, n.addTx)
	bc.onReinject = func(txs []*types.Transaction) {
		for _, tx := range txs {
			n.Net.BroadcastTx(tx, "")
		}
	}
	return n, nil
}

// addTx adds a transaction from a peer or from JSON-RPC to the pool.
func (n *Node) addTx(tx *types.Transaction) (bool, error) { return n.Pool.Add(tx) }

// Start listens, catches up with the peers, and starts mining if configured to.
func (n *Node) Start() error {
	if err := checkListen(n.cfg.Listen, n.cfg.UnsafeExpose); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", n.cfg.Listen)
	if err != nil {
		return err
	}
	n.ln, n.started = ln, time.Now()
	mux := http.NewServeMux()
	n.Net.Register(mux)
	n.registerAdmin(mux)
	n.registerRPC(mux)
	n.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go n.srv.Serve(ln) //nolint:errcheck
	n.Net.Start()

	// Catch up before mining, so a restarted node does not waste work on a stale head.
	done := make(chan struct{})
	go func() { n.Sync.SyncOnce(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		n.log.Printf("initial sync is taking long; continuing in the background")
	}
	n.Sync.Start()
	if n.cfg.Mine {
		n.Miner.Start()
	}
	head := n.Chain.CurrentBlock()
	n.log.Printf("node %s listening on http://%s, genesis %s, head %d, %d peers, mining=%v hashrate=%s",
		n.cfg.ID, n.Addr(), n.Chain.Genesis().Hash().TerminalString(), head.NumberU64(), len(n.Net.Peers()),
		n.cfg.Mine, fmtRate(n.cfg.Hashrate))
	return nil
}

// Addr is the address the node listens on.
func (n *Node) Addr() string {
	if n.ln == nil {
		return n.cfg.Listen
	}
	return n.ln.Addr().String()
}

// Close stops mining, syncing and serving.
func (n *Node) Close() {
	n.Miner.Stop()
	n.Sync.Stop()
	n.Net.Close()
	if n.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n.srv.Shutdown(ctx) //nolint:errcheck
	}
}

// checkListen refuses to expose the unauthenticated admin API beyond this machine unless
// asked to.
func checkListen(addr string, unsafe bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	if unsafe {
		log.Printf("WARNING: listening on %s: anyone who can reach it can use the admin API (stop the miner, cut peers)", addr)
		return nil
	}
	return errors.New("listen address is not loopback (127.0.0.1): the admin API has no authentication; " +
		"set \"unsafeExpose\": true in the node config if you really want this")
}

func fmtRate(r float64) string {
	switch {
	case r <= 0:
		return "unlimited"
	case r >= 1e6:
		return fmt.Sprintf("%.1fMH/s", r/1e6)
	case r >= 1e3:
		return fmt.Sprintf("%.0fkH/s", r/1e3)
	}
	return fmt.Sprintf("%.0fH/s", r)
}
