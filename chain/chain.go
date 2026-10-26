package chain

// Framework file: the BlockChain type, its block store and the helpers your insertBlock
// and reorg (blockchain.go) build on. Read it; you should not need to change it.
//
// Every block the node ever accepted is stored, canonical or not, together with its total
// difficulty and its execution results. The canonical chain is just an index on top of
// the store: number -> hash, plus tx hash -> position for transaction lookups. Your
// fork-choice code decides what that index says; the RPC server only ever reads it.

import (
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"

	"defichain/spec"
)

// InsertStatus says what InsertBlock did with a valid block.
type InsertStatus int

const (
	StatusKnown InsertStatus = iota // already stored, nothing changed
	StatusSide                      // stored, but the canonical chain did not change
	StatusCanon                     // stored, and it is the new canonical head
)

func (s InsertStatus) String() string {
	switch s {
	case StatusKnown:
		return "known"
	case StatusSide:
		return "side"
	case StatusCanon:
		return "canon"
	}
	return fmt.Sprintf("InsertStatus(%d)", int(s))
}

// txLookup is where a canonical transaction is.
type txLookup struct {
	block common.Hash
	index int
}

// BlockChain is a node's view of the block tree and of the canonical chain in it.
type BlockChain struct {
	cfg     spec.ChainConfig
	genesis *types.Block
	exec    *Executor
	pool    *TxPool // may be nil in tests
	log     *log.Logger
	now     func() time.Time // the local clock, replaceable in tests

	// insertMu serialises InsertBlock: your insertBlock runs with it held, so it never
	// races with another insertion.
	insertMu sync.Mutex

	// mu guards the store below. The helpers in this file take it themselves; do not hold
	// it when you call them.
	mu        sync.RWMutex
	blocks    map[common.Hash]*types.Block
	td        map[common.Hash]*big.Int
	results   map[common.Hash]*BlockResult
	canonical map[uint64]common.Hash // block number -> hash of the canonical block
	txIndex   map[common.Hash]txLookup
	head      *types.Block

	feedMu sync.Mutex
	subs   map[chan *types.Block]struct{}
	reorgs []spec.ReorgEvent
	nReorg int

	// onReinject is told about transactions a reorg put back into the pool (the node
	// re-broadcasts them, so that whoever mines next can include them).
	onReinject func([]*types.Transaction)

	// ext is your own extra state, declared in blockchain.go (zero value must be usable).
	ext chainExt
}

// NewBlockChain creates a chain that holds only the genesis block.
func NewBlockChain(g *spec.Genesis, pool *TxPool, logger *log.Logger) (*BlockChain, error) {
	db := newStateDatabase()
	genesis, err := GenesisBlock(g, db)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.Default()
	}
	bc := &BlockChain{
		cfg: g.Config, genesis: genesis, exec: newExecutor(g.Config, db), pool: pool, log: logger, now: time.Now,
		blocks:    map[common.Hash]*types.Block{genesis.Hash(): genesis},
		td:        map[common.Hash]*big.Int{genesis.Hash(): new(big.Int).Set(genesis.Difficulty())},
		results:   map[common.Hash]*BlockResult{genesis.Hash(): {}},
		canonical: map[uint64]common.Hash{0: genesis.Hash()},
		txIndex:   map[common.Hash]txLookup{},
		head:      genesis,
		subs:      map[chan *types.Block]struct{}{},
	}
	if pool != nil {
		pool.init(bc)
	}
	return bc, nil
}

// InsertBlock validates and stores b and applies the fork-choice rule (your insertBlock
// in blockchain.go). Safe to call from any goroutine.
func (bc *BlockChain) InsertBlock(b *types.Block) (InsertStatus, error) {
	bc.insertMu.Lock()
	defer bc.insertMu.Unlock()
	return bc.insertBlock(b)
}

// InsertChain inserts blocks in order (each one's parent must come before it) and stops
// at the first error. It returns how many were inserted and the last status.
func (bc *BlockChain) InsertChain(blocks []*types.Block) (int, InsertStatus, error) {
	status := StatusKnown
	for i, b := range blocks {
		s, err := bc.InsertBlock(b)
		if err != nil {
			return i, status, fmt.Errorf("block %d %s: %w", b.NumberU64(), b.Hash().TerminalString(), err)
		}
		status = s
	}
	return len(blocks), status, nil
}

// ---- reading the store -----------------------------------------------------------------

func (bc *BlockChain) Config() spec.ChainConfig { return bc.cfg }
func (bc *BlockChain) Genesis() *types.Block    { return bc.genesis }

// Now is the local clock (time.Now, unless a test replaced it).
func (bc *BlockChain) Now() time.Time { return bc.now() }

// SetClock replaces the local clock, for tests.
func (bc *BlockChain) SetClock(now func() time.Time) { bc.now = now }

// CurrentBlock is the head of the canonical chain.
func (bc *BlockChain) CurrentBlock() *types.Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.head
}

// GetBlock returns a stored block, canonical or not, or nil.
func (bc *BlockChain) GetBlock(hash common.Hash) *types.Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.blocks[hash]
}

// HasBlock reports whether a block is stored.
func (bc *BlockChain) HasBlock(hash common.Hash) bool { return bc.GetBlock(hash) != nil }

// GetTd returns the total difficulty of a stored block (genesis up to and including it),
// or nil if the block is not stored.
func (bc *BlockChain) GetTd(hash common.Hash) *big.Int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if td := bc.td[hash]; td != nil {
		return new(big.Int).Set(td)
	}
	return nil
}

// CanonicalHash returns the hash of canonical block n, or the zero hash.
func (bc *BlockChain) CanonicalHash(n uint64) common.Hash {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.canonical[n]
}

// GetBlockByNumber returns canonical block n, or nil.
func (bc *BlockChain) GetBlockByNumber(n uint64) *types.Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if h, ok := bc.canonical[n]; ok {
		return bc.blocks[h]
	}
	return nil
}

// IsCanonical reports whether a stored block is on the canonical chain.
func (bc *BlockChain) IsCanonical(hash common.Hash) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	b := bc.blocks[hash]
	return b != nil && bc.canonical[b.NumberU64()] == hash
}

// GetResult returns the receipts and revert reasons of a stored block.
func (bc *BlockChain) GetResult(hash common.Hash) *BlockResult {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.results[hash]
}

// GetTransaction finds a transaction on the canonical chain.
func (bc *BlockChain) GetTransaction(txHash common.Hash) (tx *types.Transaction, block *types.Block, index int) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	l, ok := bc.txIndex[txHash]
	if !ok {
		return nil, nil, 0
	}
	b := bc.blocks[l.block]
	return b.Transactions()[l.index], b, l.index
}

// TxInfo is a canonical transaction with its block and receipt.
type TxInfo struct {
	Tx      *types.Transaction
	Block   *types.Block
	Index   int
	Receipt *types.Receipt
	Revert  string // revert reason, "" if the transaction succeeded
}

// LookupTx finds a transaction on the canonical chain, with its receipt, or returns nil.
func (bc *BlockChain) LookupTx(txHash common.Hash) *TxInfo {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	l, ok := bc.txIndex[txHash]
	if !ok {
		return nil
	}
	b, res := bc.blocks[l.block], bc.results[l.block]
	info := &TxInfo{Tx: b.Transactions()[l.index], Block: b, Index: l.index}
	if res != nil && l.index < len(res.Receipts) {
		info.Receipt, info.Revert = res.Receipts[l.index], res.Reverts[l.index]
	}
	return info
}

// StateAt opens the world state after a stored block (every block's state is kept).
func (bc *BlockChain) StateAt(b *types.Block) (*state.StateDB, error) {
	return state.New(b.Root(), bc.exec.db)
}

// HeadState opens the state of the canonical head.
func (bc *BlockChain) HeadState() (*state.StateDB, *types.Block, error) {
	head := bc.CurrentBlock()
	s, err := bc.StateAt(head)
	return s, head, err
}

// Executor runs calls and gas estimates for the RPC server.
func (bc *BlockChain) Executor() *Executor { return bc.exec }

// GetHashFn serves the BLOCKHASH opcode for code running in block h: hashes of h's own
// ancestors, which on a side chain are not the canonical ones.
func (bc *BlockChain) GetHashFn(h *types.Header) vm.GetHashFunc {
	cache := []common.Hash{h.ParentHash} // cache[i] is the hash of block h.Number-1-i
	return func(n uint64) common.Hash {
		num := h.Number.Uint64()
		if n >= num {
			return common.Hash{}
		}
		for want := int(num - 1 - n); len(cache) <= want; {
			b := bc.GetBlock(cache[len(cache)-1])
			if b == nil || b.NumberU64() == 0 {
				return common.Hash{}
			}
			cache = append(cache, b.ParentHash())
		}
		return cache[num-1-n]
	}
}

// NumBlocks is how many blocks are stored, canonical or not, genesis included.
func (bc *BlockChain) NumBlocks() int {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return len(bc.blocks)
}

// AllBlocks lists every stored block (for the admin block-tree view).
func (bc *BlockChain) AllBlocks() []*types.Block {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	out := make([]*types.Block, 0, len(bc.blocks))
	for _, b := range bc.blocks {
		out = append(out, b)
	}
	return out
}

// Reorgs returns the most recent reorgs and how many there have been in total.
func (bc *BlockChain) Reorgs() ([]spec.ReorgEvent, int) {
	bc.feedMu.Lock()
	defer bc.feedMu.Unlock()
	return append([]spec.ReorgEvent(nil), bc.reorgs...), bc.nReorg
}

// SubscribeHead returns a channel that receives the new head every time it changes. It
// holds at most one block: if you are slow, you get the latest head, not every one.
func (bc *BlockChain) SubscribeHead() (<-chan *types.Block, func()) {
	ch := make(chan *types.Block, 1)
	bc.feedMu.Lock()
	bc.subs[ch] = struct{}{}
	bc.feedMu.Unlock()
	return ch, func() {
		bc.feedMu.Lock()
		delete(bc.subs, ch)
		bc.feedMu.Unlock()
	}
}

// ---- building and executing blocks -----------------------------------------------------

// BuildBlock executes txs on top of parent and returns an unsealed block. From h it uses
// Coinbase, Time, Difficulty and Extra, which you set; it fills in ParentHash, Number,
// GasLimit and BaseFee, and your build (process.go) everything that depends on the
// transactions (Root, TxHash, ReceiptHash, Bloom, GasUsed). Transactions that are invalid
// on parent's state are left out. Nonce and MixDigest are left for the seal.
func (bc *BlockChain) BuildBlock(parent *types.Block, h *types.Header, txs []*types.Transaction) (*types.Block, error) {
	if h.Difficulty == nil {
		return nil, fmt.Errorf("BuildBlock: header has no difficulty")
	}
	h = types.CopyHeader(h)
	h.ParentHash = parent.Hash()
	h.Number = new(big.Int).Add(parent.Number(), common.Big1)
	h.GasLimit = bc.cfg.GasLimit
	h.BaseFee = new(big.Int)
	return bc.exec.build(parent.Header(), h, txs, bc.GetHashFn(h))
}

// processBlock executes b on top of its parent's state and checks that the result matches
// what b's header commits to (state root, receipts, bloom, gas used), with your process
// (process.go). If it does, the new state is kept and the receipts are returned. It does not look at the PoW seal or the
// other header rules: that is VerifyHeader's job. Errors wrap ErrUnknownParent or
// ErrInvalidBlock.
func (bc *BlockChain) processBlock(b *types.Block) (*BlockResult, error) {
	parent := bc.GetBlock(b.ParentHash())
	if parent == nil {
		return nil, ErrUnknownParent
	}
	return bc.exec.process(parent.Header(), b, bc.GetHashFn(b.Header()))
}

// writeBlock stores a valid block with its total difficulty and execution results. It
// does not touch the canonical chain.
func (bc *BlockChain) writeBlock(b *types.Block, td *big.Int, res *BlockResult) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.blocks[b.Hash()] = b
	bc.td[b.Hash()] = new(big.Int).Set(td)
	bc.results[b.Hash()] = res
}

// writeCanonical makes b the canonical block at its height (replacing whatever was there)
// and indexes its transactions.
func (bc *BlockChain) writeCanonical(b *types.Block) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	n := b.NumberU64()
	if old, ok := bc.canonical[n]; ok && old != b.Hash() {
		bc.unindexTxs(old)
	}
	bc.canonical[n] = b.Hash()
	for i, tx := range b.Transactions() {
		bc.txIndex[tx.Hash()] = txLookup{block: b.Hash(), index: i}
	}
}

// deleteCanonical removes height n from the canonical chain (after a reorg to a shorter
// chain, the old heights above the new head must not stay canonical).
func (bc *BlockChain) deleteCanonical(n uint64) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if old, ok := bc.canonical[n]; ok {
		bc.unindexTxs(old)
		delete(bc.canonical, n)
	}
}

func (bc *BlockChain) unindexTxs(hash common.Hash) {
	if b := bc.blocks[hash]; b != nil {
		for _, tx := range b.Transactions() {
			if l, ok := bc.txIndex[tx.Hash()]; ok && l.block == hash {
				delete(bc.txIndex, tx.Hash())
			}
		}
	}
}

// setHead moves the head pointer to b. Call it after the canonical index is updated
// (writeCanonical / deleteCanonical). dropped are the transactions of blocks that left
// the canonical chain: they go back to the txpool, unless their nonce has been used on
// the new chain. setHead also wakes up head subscribers (the miner) and records reorgs.
func (bc *BlockChain) setHead(b *types.Block, dropped []*types.Transaction) {
	bc.mu.Lock()
	old := bc.head
	bc.head = b
	if bc.canonical[b.NumberU64()] != b.Hash() {
		bc.log.Printf("WARNING: setHead(%d %s): that block is not in the canonical index; call writeCanonical first",
			b.NumberU64(), b.Hash().TerminalString())
	}
	// A reorg is any head change that is not an extension of the old head: walk the old
	// chain back to the first block that is still canonical.
	var ev *spec.ReorgEvent
	if old.Hash() != b.Hash() {
		anc, removed := old, 0
		for anc.NumberU64() > 0 && bc.canonical[anc.NumberU64()] != anc.Hash() {
			anc = bc.blocks[anc.ParentHash()]
			removed++
		}
		if removed > 0 {
			ref := func(x *types.Block) spec.BlockRef {
				return spec.BlockRef{Number: x.NumberU64(), Hash: x.Hash(), TD: spec.NewBig(bc.td[x.Hash()])}
			}
			ev = &spec.ReorgEvent{Time: bc.now(), OldHead: ref(old), NewHead: ref(b), Ancestor: ref(anc),
				Removed: removed, Added: int(b.NumberU64() - anc.NumberU64()), DroppedTxs: len(dropped)}
		}
	}
	bc.mu.Unlock()

	if ev != nil {
		bc.log.Printf("REORG depth %d: %d %s -> %d %s (ancestor %d), %d txs back to the pool",
			ev.Removed, ev.OldHead.Number, ev.OldHead.Hash.TerminalString(), ev.NewHead.Number,
			ev.NewHead.Hash.TerminalString(), ev.Ancestor.Number, len(dropped))
	}
	if bc.pool != nil {
		if st, err := bc.StateAt(b); err == nil {
			if readded := bc.pool.reset(st, dropped); len(readded) > 0 && bc.onReinject != nil {
				bc.onReinject(readded)
			}
		}
	}
	bc.feedMu.Lock()
	if ev != nil {
		bc.nReorg++
		bc.reorgs = append(bc.reorgs, *ev)
		if len(bc.reorgs) > 256 {
			bc.reorgs = bc.reorgs[1:]
		}
	}
	for ch := range bc.subs {
		for sent := false; !sent; { // keep only the latest head in the channel
			select {
			case ch <- b:
				sent = true
			default:
				select {
				case <-ch:
				default:
				}
			}
		}
	}
	bc.feedMu.Unlock()
}
