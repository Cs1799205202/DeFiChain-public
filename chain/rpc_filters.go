package chain

// Framework file: eth_getLogs and polling filters (eth_newFilter & co).
//
// A log filter reports new logs as blocks join the canonical chain. When a reorg takes
// blocks off the canonical chain, their logs are reported again with "removed": true, as
// geth does: an agent that reacts to events must handle that.

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	maxLogRange   = 10_000          // blocks per eth_getLogs
	filterTimeout = 5 * time.Minute // filters not polled for this long are removed
	filterHistory = 128             // canonical blocks a filter remembers, to detect reorgs
)

// addrList is null, one address, or a list.
type addrList []common.Address

func (a *addrList) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*a = nil
		return nil
	}
	var one common.Address
	if err := json.Unmarshal(data, &one); err == nil {
		*a = addrList{one}
		return nil
	}
	var many []common.Address
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("address must be an address or a list of addresses")
	}
	*a = many
	return nil
}

// topicList is one topic position: null (anything), one hash, or alternatives.
type topicList []common.Hash

func (t *topicList) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*t = nil
		return nil
	}
	var one common.Hash
	if err := json.Unmarshal(data, &one); err == nil {
		*t = topicList{one}
		return nil
	}
	var many []*common.Hash
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("topics must be null, a hash or a list of hashes")
	}
	*t = nil
	for _, h := range many {
		if h == nil { // [A, null] means "A or anything": anything
			*t = nil
			return nil
		}
		*t = append(*t, *h)
	}
	return nil
}

type logQuery struct {
	FromBlock *blockParam  `json:"fromBlock"`
	ToBlock   *blockParam  `json:"toBlock"`
	BlockHash *common.Hash `json:"blockHash"`
	Address   addrList     `json:"address"`
	Topics    []topicList  `json:"topics"`
}

func (q *logQuery) match(l *types.Log) bool {
	if len(q.Address) > 0 {
		ok := false
		for _, a := range q.Address {
			ok = ok || a == l.Address
		}
		if !ok {
			return false
		}
	}
	if len(q.Topics) > len(l.Topics) {
		return false
	}
	for i, alts := range q.Topics {
		if len(alts) == 0 {
			continue
		}
		ok := false
		for _, h := range alts {
			ok = ok || h == l.Topics[i]
		}
		if !ok {
			return false
		}
	}
	return true
}

// blockLogs returns b's logs that match q (marked removed if asked).
func (n *Node) blockLogs(b *types.Block, q *logQuery, removed bool) []*types.Log {
	res := n.Chain.GetResult(b.Hash())
	if res == nil {
		return nil
	}
	var out []*types.Log
	for _, r := range res.Receipts {
		for _, l := range r.Logs {
			if q.match(l) {
				if removed {
					cp := *l
					cp.Removed = true
					l = &cp
				}
				out = append(out, l)
			}
		}
	}
	return out
}

func (n *Node) rpcGetLogs(params json.RawMessage) (any, *rpcError) {
	var q logQuery
	if err := args(params, 1, &q); err != nil {
		return nil, err
	}
	return n.queryLogs(&q)
}

func (n *Node) queryLogs(q *logQuery) (any, *rpcError) {
	out := []*types.Log{}
	if q.BlockHash != nil {
		b := n.Chain.GetBlock(*q.BlockHash)
		if b == nil {
			return nil, &rpcError{Code: -32000, Message: "unknown block"}
		}
		return append(out, n.blockLogs(b, q, false)...), nil
	}
	from, to := n.resolve(q.FromBlock), n.resolve(q.ToBlock)
	if from == nil || to == nil || from.NumberU64() > to.NumberU64() {
		return out, nil
	}
	if to.NumberU64()-from.NumberU64() >= maxLogRange {
		return nil, &rpcError{Code: -32005, Message: fmt.Sprintf("query exceeds max block range %d", maxLogRange)}
	}
	for i := from.NumberU64(); i <= to.NumberU64(); i++ {
		b := n.Chain.GetBlockByNumber(i)
		if b == nil {
			break
		}
		out = append(out, n.blockLogs(b, q, false)...)
	}
	return out, nil
}

// ---- polling filters -------------------------------------------------------------------

const (
	logFilter = iota
	blockFilter
	pendingFilter
)

type seenBlock struct {
	num  uint64
	hash common.Hash
}

type filter struct {
	kind     int
	query    logQuery
	seen     []seenBlock // canonical blocks already reported, oldest first
	pending  map[common.Hash]bool
	lastPoll time.Time
}

type filterSet struct {
	mu   sync.Mutex
	next uint64
	m    map[string]*filter
}

func newFilterSet() *filterSet { return &filterSet{m: map[string]*filter{}} }

func (n *Node) rpcFilter(method string, params json.RawMessage) (any, *rpcError) {
	fs := n.filters
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for id, f := range fs.m {
		if time.Since(f.lastPoll) > filterTimeout {
			delete(fs.m, id)
		}
	}
	switch method {
	case "eth_newFilter", "eth_newBlockFilter", "eth_newPendingTransactionFilter":
		f := &filter{lastPoll: time.Now()}
		switch method {
		case "eth_newFilter":
			f.kind = logFilter
			if err := args(params, 1, &f.query); err != nil {
				return nil, err
			}
		case "eth_newBlockFilter":
			f.kind = blockFilter
		default:
			f.kind = pendingFilter
			f.pending = map[common.Hash]bool{}
			for _, tx := range n.Pool.Pending() {
				f.pending[tx.Hash()] = true
			}
		}
		head := n.Chain.CurrentBlock()
		f.seen = []seenBlock{{head.NumberU64(), head.Hash()}}
		fs.next++
		id := hexutil.EncodeUint64(fs.next)
		fs.m[id] = f
		return id, nil
	}
	var id string
	if err := args(params, 1, &id); err != nil {
		return nil, err
	}
	f := fs.m[id]
	if method == "eth_uninstallFilter" {
		delete(fs.m, id)
		return f != nil, nil
	}
	if f == nil {
		return nil, &rpcError{Code: -32000, Message: "filter not found"}
	}
	f.lastPoll = time.Now()
	if method == "eth_getFilterLogs" {
		if f.kind != logFilter {
			return nil, &rpcError{Code: -32000, Message: "filter not found"}
		}
		return n.queryLogs(&f.query)
	}
	return n.filterChanges(f), nil
}

// filterChanges reports what happened since the last poll.
func (n *Node) filterChanges(f *filter) any {
	bc := n.Chain
	if f.kind == pendingFilter {
		out := []common.Hash{}
		now := map[common.Hash]bool{}
		for _, tx := range n.Pool.Pending() {
			now[tx.Hash()] = true
			if !f.pending[tx.Hash()] {
				out = append(out, tx.Hash())
			}
		}
		f.pending = now
		return out
	}
	logs := []*types.Log{}
	hashes := []common.Hash{}
	// Blocks we reported that are no longer canonical: a reorg happened.
	var lowest uint64
	removedAny := false
	for len(f.seen) > 0 {
		last := f.seen[len(f.seen)-1]
		if bc.CanonicalHash(last.num) == last.hash {
			break
		}
		f.seen = f.seen[:len(f.seen)-1]
		lowest, removedAny = last.num, true
		if f.kind == logFilter {
			if b := bc.GetBlock(last.hash); b != nil {
				logs = append(logs, n.blockLogs(b, &f.query, true)...)
			}
		}
	}
	start := lowest
	if len(f.seen) > 0 {
		start = f.seen[len(f.seen)-1].num + 1
	} else if !removedAny {
		start = bc.CurrentBlock().NumberU64() + 1
	}
	head := bc.CurrentBlock().NumberU64()
	for i := start; i <= head; i++ {
		b := bc.GetBlockByNumber(i)
		if b == nil {
			break
		}
		f.seen = append(f.seen, seenBlock{i, b.Hash()})
		if f.kind == logFilter {
			logs = append(logs, n.blockLogs(b, &f.query, false)...)
		} else {
			hashes = append(hashes, b.Hash())
		}
	}
	if len(f.seen) > filterHistory {
		f.seen = f.seen[len(f.seen)-filterHistory:]
	}
	if f.kind == logFilter {
		return logs
	}
	return hashes
}
