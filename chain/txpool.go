package chain

// ★ Student file (Part 1, task 4): the transaction pool (mempool).
//
// The pool holds valid transactions that are not on the canonical chain yet. Every node
// has one; transactions are gossiped to all of them, so whoever mines next can include
// them. A transaction leaves the pool when the canonical chain uses up its nonce, and
// comes back if a reorg takes it off the canonical chain again (setHead calls reset).
//
// Per sender, transactions are ordered by nonce, and only the run of consecutive nonces
// starting at the sender's nonce on the head state can be executed now ("pending"); the
// others wait for the missing nonce ("queued"). Callers: the RPC server (Add, Get,
// PendingNonce, Content), the P2P layer (Add, Has), the miner (Pending) and setHead
// (reset), all from different goroutines.
//
// Helpers: p.bc.exec.ValidateTx (executor.go), p.bc.HeadState (chain.go).
// Tests: go test ./chain -run TestTxPool -v

import (
	"container/heap"
	"fmt"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

// Packages you will probably need (delete these lines once you use them).
var (
	_ = heap.Init
	_ = fmt.Errorf
	_ = sort.Slice
	_ = core.ErrNonceTooLow
)

// TxPool is the pool. Add any fields you need; NewTxPool must return a usable pool.
//
// TODO: choose the data structures. You need, quickly: a transaction by hash (Has, Get,
// duplicates); a sender's transaction by nonce (replacement, pending runs); the arrival
// order (ties in Pending). Every method can be called from several goroutines at once:
// guard the fields with mu.
type TxPool struct {
	mu sync.Mutex
	bc *BlockChain
}

// NewTxPool creates an empty pool; NewBlockChain attaches it to the chain.
func NewTxPool() *TxPool {
	return &TxPool{}
}

// init is called once by NewBlockChain, before the pool is used.
func (p *TxPool) init(bc *BlockChain) { p.bc = bc }

// Add validates tx and adds it. It returns (true, nil) if tx was added, (false, nil) if
// the pool already had it, and an error if tx is rejected. In order:
//
//   - the stateless rules (ValidateTx): its error;
//   - already in the pool (same hash): false, nil;
//   - nonce below the sender's nonce on the head state: core.ErrNonceTooLow;
//   - balance on the head state below tx.Cost() (gas * fee cap + value): core.ErrInsufficientFunds;
//   - same sender and nonce as a pooled transaction: tx replaces it if its tip
//     (GasTipCap) is strictly higher, otherwise ErrReplaceUnderpriced;
//   - the pool already holds MaxPoolTxs transactions (and tx replaces none): ErrPoolFull.
//
// A nonce gap is fine: tx waits in the queue until the missing nonces arrive.
//
// TODO. Wrap the go-ethereum errors so that errors.Is finds them, e.g.
// fmt.Errorf("%w: tx %d, account %d", core.ErrNonceTooLow, tx.Nonce(), next).
func (p *TxPool) Add(tx *types.Transaction) (bool, error) {
	return false, ErrNotImplemented
}

// Has reports whether a transaction is in the pool.
func (p *TxPool) Has(hash common.Hash) bool {
	return false
}

// Get returns a pooled transaction, or nil.
func (p *TxPool) Get(hash common.Hash) *types.Transaction {
	return nil
}

// Len is the number of pooled transactions, pending and queued.
func (p *TxPool) Len() int {
	return 0
}

// Pending returns the transactions that can go into the next block, in the order a miner
// should try them: per sender, its pending run in nonce order; across senders, always
// the highest tip (GasTipCap) among the senders' next transactions, and on equal tips
// the one that arrived first. The ordering is a miner's policy, not a consensus rule
// (Part 3 may explore others), but the tests and the DeFi experiments expect this one.
//
// TODO. A heap (container/heap) of the senders' runs, ordered by their first
// transaction, gives this order without sorting everything again after each pick.
func (p *TxPool) Pending() []*types.Transaction {
	return nil
}

// PendingNonce is the nonce the sender's next transaction should use: its nonce on the
// head state plus the length of its pending run (eth_getTransactionCount "pending").
func (p *TxPool) PendingNonce(addr common.Address) uint64 {
	return 0
}

// Content splits the pool into pending and queued transactions, per sender, each list
// sorted by nonce (txpool_content). A sender with no transaction of a kind has no entry
// in that map.
func (p *TxPool) Content() (pending, queued map[common.Address][]*types.Transaction) {
	return map[common.Address][]*types.Transaction{}, map[common.Address][]*types.Transaction{}
}

// reset adapts the pool to a new head whose state is st. setHead calls it after every
// head change, with dropped = the transactions of the blocks that left the canonical
// chain (none unless there was a reorg), oldest first. Each dropped transaction goes
// through the same checks as Add, but against st; those that pass are added back (a
// dropped transaction whose nonce the new chain has used is already in the new chain, or
// replaced). Then every pooled transaction whose nonce is below its sender's nonce on st
// is removed. It returns the dropped transactions that were added back, in order.
//
// TODO. Share the checks with Add: a helper that takes the state to check against.
func (p *TxPool) reset(st *state.StateDB, dropped []*types.Transaction) []*types.Transaction {
	return nil
}
