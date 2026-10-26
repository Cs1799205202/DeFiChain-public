package chain

// ★ Student file (Part 1, task 5): inserting blocks, fork choice and reorgs.
//
// The node keeps every valid block it has seen, as a tree rooted at genesis. Fork choice
// picks one leaf as the head; the canonical chain is the path from genesis to it. Our
// rule is Nakamoto's: the head is the block with the highest total difficulty (TD, the
// sum of the difficulties of all blocks from genesis to it: "the most work"), not the
// longest chain. On a tie, keep the current head (first seen wins).
//
// Helpers you will use, all in chain.go: processBlock, writeBlock, writeCanonical,
// deleteCanonical, setHead, GetBlock, GetTd, CurrentBlock, CanonicalHash.
// Tests: go test ./chain -run TestChain -v

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Packages you will probably need (delete these lines once you use them).
var (
	_ = errors.New
	_ = fmt.Errorf
	_ = big.NewInt
	_ = common.Hash{}
)

// chainExt is room for your own state in BlockChain (bc.ext). Its zero value must be
// ready to use. The reference solution needs none.
type chainExt struct{}

// insertBlock validates b, stores it, and makes it the head if it has the highest total
// difficulty. It runs with bc.insertMu held (see InsertBlock), so no other insertion runs
// at the same time. In order:
//
//  1. Already stored?                       -> StatusKnown, nil
//  2. Parent not stored?                    -> ErrUnknownParent (the caller fetches it)
//  3. VerifyHeader(header, parent, now)     -> its error (now = bc.Now().Unix())
//  4. processBlock(b)                       -> its error (wraps ErrInvalidBlock)
//  5. writeBlock(b, td, result) with td = TD(parent) + b.Difficulty
//  6. Fork choice: if td > TD(current head), reorg to b -> StatusCanon; else StatusSide.
//
// An invalid block must not be stored, and must not change the head.
//
// TODO. Do not call InsertBlock or InsertChain from here: bc.insertMu is already held, it
// would deadlock. Use bc.Now(), not time.Now(): tests set the clock.
func (bc *BlockChain) insertBlock(b *types.Block) (InsertStatus, error) {
	return 0, ErrNotImplemented
}

// reorg makes newHead the head of the canonical chain, in place of oldHead. Usually
// newHead is a child of oldHead and nothing is taken away; but it may also be on another
// branch of the tree, and then:
//
//   - find the common ancestor of the two heads (walk both back by parent hash; walk the
//     higher one down first, until both are at the same height);
//   - every block from the ancestor (exclusive) to newHead becomes canonical (writeCanonical);
//   - heights above newHead that were canonical on the old chain stop being canonical
//     (deleteCanonical), or GetBlockByNumber would still return old blocks there;
//   - transactions of the old branch that are not in the new one go back to the txpool:
//     pass them to setHead, oldest block first, in block order.
//
// Finally setHead(newHead, dropped).
//
// TODO. Draw the two branches on paper first, for the case where newHead is lower than
// oldHead (a shorter but heavier chain): which heights must change, which must disappear?
func (bc *BlockChain) reorg(oldHead, newHead *types.Block) error {
	return ErrNotImplemented
}
