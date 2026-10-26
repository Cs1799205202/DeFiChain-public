package chain

// ★ Student file (Part 1, task 3): executing and validating blocks.
//
// A block header commits to the result of running its transactions on the parent's
// state: Root (the world state after the block), GasUsed, ReceiptHash and Bloom (the
// receipts and their logs), and TxHash (the transactions themselves). A miner computes
// these fields when it builds a block (build); every node runs the block again when it
// receives it, and rejects it unless it gets exactly the same values (process). That is
// why every node can trust the state without trusting the miner.
//
// Running a block is: for each transaction in order, applyTx (the EVM, in executor.go);
// then the block reward to the coinbase (payReward); then stateRoot. Mind the difference
// between an invalid transaction (applyTx returns an error: wrong nonce, cannot pay,
// does not fit the block's gas, ...), which can never be in a block, and a reverted one,
// which is a normal transaction with a failed receipt (Status 0) that still pays its gas.
//
// Helpers (executor.go): openState, newGasPool, applyTx, payReward, stateRoot,
// commitState, txsRoot, receiptsRoot, logsBloom, assemble.
// Tests: go test ./chain -run TestExec -v

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// Packages you will probably need (delete these lines once you use them).
var (
	_ = fmt.Errorf
	_ = common.Hash{}
	_ = (*state.StateDB)(nil)
)

// execResult is a suggestion: what running a list of transactions produced. Change it
// as you like; only build and process are called from outside this file.
type execResult struct {
	txs      []*types.Transaction // the transactions that were executed
	receipts []*types.Receipt
	reverts  []string // per transaction: its revert reason, or ""
	gasUsed  uint64
}

// build executes candidate transactions on top of parent and returns the unsealed block.
// h has every field set except those that depend on execution: build fills in Root,
// GasUsed, TxHash, ReceiptHash and Bloom (UncleHash is the empty hash). Invalid
// transactions are left out, in place: the others are tried in the given order, so a
// transaction that does not fit in the gas left does not stop a smaller one after it.
// Nothing is written to the state database: the block is executed again, for real, by
// process when it is inserted (so the receipts here have a zero block hash; that is fine).
//
// TODO:
//  1. Open the parent's state (openState(parent.Root)) and make the block's gas pool
//     (newGasPool(h)).
//  2. applyTx each candidate in order. Pass the number of transactions included so far
//     as the index, not the position in txs. On an error, leave the transaction out.
//  3. payReward, after all transactions (a coinbase cannot spend the reward of the block
//     it is mining).
//  4. Fill in h.Root (stateRoot) and h.GasUsed (gp.CumulativeUsed()); assemble computes
//     the rest. Work on a copy of h (types.CopyHeader): the caller owns it.
//
// build and process run the same loop; you may share it (in a function like execute,
// with a "strict" flag), but get build working first: the tests of the later tasks use it.
func (x *Executor) build(parent *types.Header, h *types.Header, txs []*types.Transaction, getHash vm.GetHashFunc) (*types.Block, error) {
	return nil, ErrNotImplemented
}

// process executes block b on top of parent and checks every commitment in b's header.
// Only if they all hold is the new state committed (commitState) and the result
// returned. Every reason to reject the block is an error wrapping ErrInvalidBlock.
//
// TODO:
//  1. The body must match the header: no uncles, no withdrawals (b.Uncles(),
//     b.Withdrawals() must be empty and nil), and txsRoot(b.Transactions()) == TxHash.
//  2. Run the transactions like build does, but strictly: one invalid transaction makes
//     the whole block invalid (do not skip it: a block that skipped it would have the
//     same header as one without it). Use b.Hash() as the block hash, so that receipts
//     and logs point to their block.
//  3. payReward; then compare GasUsed, Bloom (logsBloom), ReceiptHash (receiptsRoot) and
//     Root (stateRoot) with the header.
//  4. Only then commitState, and return &BlockResult{Receipts, Reverts}: one receipt
//     and one revert reason ("" if it succeeded) per transaction, in block order.
func (x *Executor) process(parent *types.Header, b *types.Block, getHash vm.GetHashFunc) (*BlockResult, error) {
	return nil, ErrNotImplemented
}
