package chain

// Framework file: the peer-to-peer layer, over plain HTTP, with fault injection.
//
// Real blockchains use TCP connections and a gossip protocol (devp2p, Bitcoin's p2p). Here
// every message is one HTTP request between nodes, which is slower but easy to watch,
// debug and break on purpose:
//
//	POST /p2p/block                      push a block (RLP)            -> your Handler.HandleBlock
//	POST /p2p/tx                         push a transaction            -> txpool, then relayed
//	GET  /p2p/head                       the sender's canonical head   <- Network.RequestHead
//	GET  /p2p/ancestors?hash=H&max=N     block H and its ancestors     <- Network.RequestAncestors
//
// Every request carries the sender's id (X-Node-Id) and genesis hash (X-Genesis): a node
// rejects requests from another network, and from peers it is "partitioned" from.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"defichain/spec"
)

// ErrPeerBlocked: fault injection says this node cannot reach that peer.
var ErrPeerBlocked = errors.New("peer unreachable (blocked by fault injection)")

// PeerHead is a peer's canonical head as it reports it. Nothing checks that the peer is
// telling the truth: use it to decide whether to fetch blocks, never as proof of anything.
type PeerHead struct {
	Number uint64
	Hash   common.Hash
	TD     *big.Int
}

// PeerNet is what the sync handler and the miner use of the network. *Network implements
// it; tests use a fake.
type PeerNet interface {
	// Peers lists the ids of this node's direct peers, sorted. Some may be unreachable.
	Peers() []string
	// BroadcastBlock pushes b to every peer except `except` (use "" to send to all). It
	// returns immediately; delivery is best-effort.
	BroadcastBlock(b *types.Block, except string)
	// SendBlock pushes b to one peer, best-effort.
	SendBlock(peer string, b *types.Block)
	// RequestHead asks a peer for its canonical head.
	RequestHead(peer string) (PeerHead, error)
	// RequestAncestors asks a peer for the block with this hash and up to max-1 of its
	// ancestors, newest first: [hash, parent, grandparent, ...]. The list stops early at
	// block 1 (genesis is never sent) and is capped at MaxAncestors blocks.
	RequestAncestors(peer string, hash common.Hash, max int) ([]*types.Block, error)
}

// MaxAncestors is the most blocks one RequestAncestors returns.
const MaxAncestors = 256

// Network is one node's p2p endpoint: a client for talking to its peers and the HTTP
// handlers for being talked to.
type Network struct {
	id      string
	genesis common.Hash
	bc      *BlockChain
	log     *log.Logger
	client  *http.Client
	verbose bool

	mu      sync.RWMutex
	peers   map[string]string // id -> base URL
	cond    spec.NetConditions
	blocked map[string]bool
	rnd     *rand.Rand

	inbox   chan inbound
	onBlock func(b *types.Block, from string)
	onTx    func(tx *types.Transaction) (bool, error)
	stop    chan struct{}
	once    sync.Once
}

type inbound struct {
	block *types.Block
	from  string
}

// NewNetwork creates the p2p endpoint of node id.
func NewNetwork(id string, peers map[string]string, bc *BlockChain, logger *log.Logger) *Network {
	n := &Network{
		id: id, genesis: bc.Genesis().Hash(), bc: bc, log: logger,
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 16}},
		peers:  map[string]string{}, blocked: map[string]bool{},
		rnd:   rand.New(rand.NewSource(time.Now().UnixNano())),
		inbox: make(chan inbound, 1024), stop: make(chan struct{}),
	}
	for pid, url := range peers {
		if pid != id {
			n.peers[pid] = url
		}
	}
	return n
}

// SetHandlers wires incoming blocks (to the sync handler) and transactions (to the
// txpool). Call before Start.
func (n *Network) SetHandlers(onBlock func(*types.Block, string), onTx func(*types.Transaction) (bool, error)) {
	n.onBlock, n.onTx = onBlock, onTx
}

// Start processes incoming blocks, one at a time, in arrival order.
func (n *Network) Start() {
	go func() {
		for {
			select {
			case <-n.stop:
				return
			case in := <-n.inbox:
				if n.onBlock != nil {
					n.onBlock(in.block, in.from)
				}
			}
		}
	}()
}

// Close stops processing incoming blocks.
func (n *Network) Close() { n.once.Do(func() { close(n.stop) }) }

// ID is this node's id.
func (n *Network) ID() string { return n.id }

// ---- fault injection -------------------------------------------------------------------

// Conditions returns the current fault injection settings.
func (n *Network) Conditions() spec.NetConditions {
	n.mu.RLock()
	defer n.mu.RUnlock()
	c := n.cond
	c.Blocked = append([]string(nil), c.Blocked...)
	return c
}

// SetConditions replaces the fault injection settings.
func (n *Network) SetConditions(c spec.NetConditions) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cond = c
	n.blocked = map[string]bool{}
	for _, id := range c.Blocked {
		n.blocked[id] = true
	}
}

// AddPeer adds (or updates) a direct peer.
func (n *Network) AddPeer(id, url string) {
	if id == n.id {
		return
	}
	n.mu.Lock()
	n.peers[id] = url
	n.mu.Unlock()
}

// RemovePeer forgets a peer.
func (n *Network) RemovePeer(id string) {
	n.mu.Lock()
	delete(n.peers, id)
	n.mu.Unlock()
}

// route returns where to send a message to peer, and how long to hold it first.
func (n *Network) route(peer string) (url string, delay time.Duration, drop bool, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	url = n.peers[peer]
	switch {
	case url == "":
		return "", 0, false, fmt.Errorf("unknown peer %q", peer)
	case n.blocked[peer]:
		return "", 0, false, ErrPeerBlocked
	}
	delay = n.cond.Delay.Std()
	if j := n.cond.Jitter.Std(); j > 0 {
		delay += time.Duration(n.rnd.Int63n(int64(j)))
	}
	drop = n.cond.Drop > 0 && n.rnd.Float64() < n.cond.Drop
	return url, delay, drop, nil
}

// ---- client side: the PeerNet interface ------------------------------------------------

// Peers lists the direct peers' ids, sorted.
func (n *Network) Peers() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.peers))
	for id := range n.peers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// BroadcastBlock pushes b to all peers except `except`.
func (n *Network) BroadcastBlock(b *types.Block, except string) {
	data, err := rlp.EncodeToBytes(b)
	if err != nil {
		n.log.Printf("p2p: encode block: %v", err)
		return
	}
	for _, peer := range n.Peers() {
		if peer != except {
			n.push(peer, "/p2p/block", data)
		}
	}
}

// SendBlock pushes b to one peer.
func (n *Network) SendBlock(peer string, b *types.Block) {
	if data, err := rlp.EncodeToBytes(b); err == nil {
		n.push(peer, "/p2p/block", data)
	}
}

// BroadcastTx pushes a transaction to all peers except `except`.
func (n *Network) BroadcastTx(tx *types.Transaction, except string) {
	data, err := tx.MarshalBinary()
	if err != nil {
		return
	}
	for _, peer := range n.Peers() {
		if peer != except {
			n.push(peer, "/p2p/tx", data)
		}
	}
}

// push delivers a message in the background, after the simulated delay; it may be dropped.
func (n *Network) push(peer, path string, data []byte) {
	url, delay, drop, err := n.route(peer)
	if err != nil || drop {
		return
	}
	go func() {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-n.stop:
				return
			}
		}
		resp, err := n.send(http.MethodPost, url+path, data)
		if err != nil {
			return // best-effort: the receiver catches up by syncing
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
	}()
}

func (n *Network) send(method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Node-Id", n.id)
	req.Header.Set("X-Genesis", n.genesis.Hex())
	req.Header.Set("Content-Type", "application/octet-stream")
	return n.client.Do(req)
}

// request is a synchronous call to a peer, after the simulated delay.
func (n *Network) request(peer, path string) ([]byte, error) {
	url, delay, _, err := n.route(peer)
	if err != nil {
		return nil, err
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	resp, err := n.send(http.MethodGet, url+path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s: %s: %s", peer, resp.Status, bytes.TrimSpace(data))
	}
	return data, nil
}

// RequestHead asks a peer for its canonical head.
func (n *Network) RequestHead(peer string) (PeerHead, error) {
	data, err := n.request(peer, "/p2p/head")
	if err != nil {
		return PeerHead{}, err
	}
	var ref spec.BlockRef
	if err := json.Unmarshal(data, &ref); err != nil || ref.TD == nil {
		return PeerHead{}, fmt.Errorf("peer %s: bad head: %.100s", peer, data)
	}
	return PeerHead{Number: ref.Number, Hash: ref.Hash, TD: ref.TD.Int()}, nil
}

// RequestAncestors asks a peer for a block and its ancestors, newest first.
func (n *Network) RequestAncestors(peer string, hash common.Hash, max int) ([]*types.Block, error) {
	data, err := n.request(peer, fmt.Sprintf("/p2p/ancestors?hash=%s&max=%d", hash.Hex(), max))
	if err != nil {
		return nil, err
	}
	var blocks []*types.Block
	if err := rlp.DecodeBytes(data, &blocks); err != nil {
		return nil, fmt.Errorf("peer %s: decode blocks: %w", peer, err)
	}
	return blocks, nil
}

// ---- server side -----------------------------------------------------------------------

// Register mounts the /p2p/ handlers.
func (n *Network) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /p2p/block", n.handleBlock)
	mux.HandleFunc("POST /p2p/tx", n.handleTx)
	mux.HandleFunc("GET /p2p/head", n.handleHead)
	mux.HandleFunc("GET /p2p/ancestors", n.handleAncestors)
}

// admit checks that a request comes from a reachable node of the same network.
func (n *Network) admit(w http.ResponseWriter, r *http.Request) (string, bool) {
	from := r.Header.Get("X-Node-Id")
	if g := r.Header.Get("X-Genesis"); g != "" && common.HexToHash(g) != n.genesis {
		http.Error(w, "different genesis block: that node is on another network", http.StatusConflict)
		return "", false
	}
	n.mu.RLock()
	blocked := n.blocked[from]
	n.mu.RUnlock()
	if blocked {
		http.Error(w, ErrPeerBlocked.Error(), http.StatusServiceUnavailable)
		return "", false
	}
	return from, true
}

func (n *Network) handleBlock(w http.ResponseWriter, r *http.Request) {
	from, ok := n.admit(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b := new(types.Block)
	if err := rlp.DecodeBytes(data, b); err != nil {
		http.Error(w, "decode block: "+err.Error(), http.StatusBadRequest)
		return
	}
	if n.verbose {
		n.log.Printf("p2p: block %d %s from %s", b.NumberU64(), b.Hash().TerminalString(), from)
	}
	select {
	case n.inbox <- inbound{block: b, from: from}:
		w.WriteHeader(http.StatusAccepted)
	default:
		n.log.Printf("p2p: inbox full, dropped block %d from %s", b.NumberU64(), from)
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}
}

func (n *Network) handleTx(w http.ResponseWriter, r *http.Request) {
	from, ok := n.admit(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(data); err != nil {
		http.Error(w, "decode tx: "+err.Error(), http.StatusBadRequest)
		return
	}
	if n.onTx == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	added, err := n.onTx(tx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if added {
		n.BroadcastTx(tx, from) // relay, so transactions cross sparse topologies too
	}
	w.WriteHeader(http.StatusAccepted)
}

func (n *Network) handleHead(w http.ResponseWriter, r *http.Request) {
	if _, ok := n.admit(w, r); !ok {
		return
	}
	head := n.bc.CurrentBlock()
	writeJSON(w, http.StatusOK, spec.BlockRef{Number: head.NumberU64(), Hash: head.Hash(), TD: spec.NewBig(n.bc.GetTd(head.Hash()))})
}

func (n *Network) handleAncestors(w http.ResponseWriter, r *http.Request) {
	if _, ok := n.admit(w, r); !ok {
		return
	}
	max, _ := strconv.Atoi(r.URL.Query().Get("max"))
	if max <= 0 || max > MaxAncestors {
		max = MaxAncestors
	}
	b := n.bc.GetBlock(common.HexToHash(r.URL.Query().Get("hash")))
	if b == nil {
		http.Error(w, "unknown block", http.StatusNotFound)
		return
	}
	var out []*types.Block
	for b != nil && b.NumberU64() > 0 && len(out) < max {
		out = append(out, b)
		b = n.bc.GetBlock(b.ParentHash())
	}
	data, err := rlp.EncodeToBytes(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data) //nolint:errcheck
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
