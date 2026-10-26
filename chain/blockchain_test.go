package chain

// Tests for task 5 (blockchain.go). Run: go test ./chain -run TestChain -v

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestChainInsertLinear(t *testing.T) {
	bc, lb := labChain(t), labNewBuilder(t)
	tx := lb.tx(labAliceKey, 0, labBob, 12345)
	b1 := lb.next(lb.genesis(), 2, labMinerA, tx)
	blocks := append([]*types.Block{b1}, lb.chain(b1, 4, 2, labMinerA)...)
	for _, b := range blocks {
		labMustInsert(t, bc, b, StatusCanon)
		if got, want := bc.GetTd(b.Hash()), lb.td(b); got == nil || got.Cmp(want) != 0 {
			t.Fatalf("GetTd(block %d) = %v, want %v (TD of the parent + this block's difficulty)", b.NumberU64(), got, want)
		}
	}
	labWantCanonical(t, bc, blocks[len(blocks)-1])
	if info := bc.LookupTx(tx.Hash()); info == nil || info.Block.Hash() != b1.Hash() || info.Receipt == nil || info.Receipt.Status != 1 {
		t.Fatalf("LookupTx(transfer in block 1) = %+v: not indexed (writeCanonical indexes a block's transactions)", info)
	}
	for _, b := range blocks {
		labMustInsert(t, bc, b, StatusKnown)
	}
	labWantCanonical(t, bc, blocks[len(blocks)-1])
}

func TestChainRejectsInvalid(t *testing.T) {
	bc, lb := labChain(t), labNewBuilder(t)
	b1 := lb.next(lb.genesis(), 2, labMinerA)
	b2 := lb.next(b1, 2, labMinerA)

	if _, err := labInsert(t, bc, b2); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("InsertBlock(block whose parent is unknown) = %v, want ErrUnknownParent", err)
	}
	if bc.HasBlock(b2.Hash()) {
		t.Fatal("a block with an unknown parent was stored")
	}

	// Valid PoW, wrong state root: only executing the block finds out.
	h := b1.Header()
	h.Root[0] ^= 0xff
	badRoot := types.NewBlockWithHeader(labReseal(t, h)).WithBody(*b1.Body())
	if _, err := labInsert(t, bc, badRoot); !errors.Is(err, ErrInvalidBlock) {
		t.Fatalf("InsertBlock(wrong state root) = %v, want ErrInvalidBlock", err)
	}
	// Wrong seal.
	h = b1.Header()
	h.Nonce = types.EncodeNonce(h.Nonce.Uint64() + 1)
	if _, err := labInsert(t, bc, b1.WithSeal(h)); !errors.Is(err, ErrInvalidMixDigest) {
		t.Fatalf("InsertBlock(wrong seal) = %v, want ErrInvalidMixDigest", err)
	}
	if bc.CurrentBlock().NumberU64() != 0 || bc.HasBlock(badRoot.Hash()) {
		t.Fatal("an invalid block was stored or changed the head")
	}
	labMustInsert(t, bc, b1, StatusCanon)
	labMustInsert(t, bc, b2, StatusCanon)
}

func TestChainForkChoice(t *testing.T) {
	bc, lb := labChain(t), labNewBuilder(t)
	// Two branches from genesis with blocks 2 s apart: equal difficulties, so total
	// difficulty grows with length here.
	a := lb.chain(lb.genesis(), 3, 2, labMinerA)
	b := lb.chain(lb.genesis(), 4, 2, labMinerB)
	for _, blk := range a {
		labMustInsert(t, bc, blk, StatusCanon)
	}
	for i, blk := range b[:3] {
		labMustInsert(t, bc, blk, StatusSide) // not heavier: B1 < A3, B2 < A3, B3 == A3 (a tie keeps A)
		if i == 2 && lb.td(blk).Cmp(lb.td(a[2])) != 0 {
			t.Fatal("test bug: the branches should tie at height 3")
		}
	}
	labWantCanonical(t, bc, a[2])
	labMustInsert(t, bc, b[3], StatusCanon) // heavier: reorg to B
	labWantCanonical(t, bc, b[3])
	for _, blk := range a {
		if !bc.HasBlock(blk.Hash()) || bc.IsCanonical(blk.Hash()) {
			t.Fatalf("block %d of the old branch: stored=%v canonical=%v, want stored and not canonical",
				blk.NumberU64(), bc.HasBlock(blk.Hash()), bc.IsCanonical(blk.Hash()))
		}
	}
}

func TestChainReorgMovesTransactions(t *testing.T) {
	bc, lb := labChain(t), labNewBuilder(t)
	tx := lb.tx(labAliceKey, 0, labBob, 1_000_000)
	a1 := lb.next(lb.genesis(), 2, labMinerA, tx)
	b := lb.chain(lb.genesis(), 2, 2, labMinerB) // no transactions
	labMustInsert(t, bc, a1, StatusCanon)
	labMustInsert(t, bc, b[0], StatusSide)
	labMustInsert(t, bc, b[1], StatusCanon)
	labWantCanonical(t, bc, b[1])
	if bc.LookupTx(tx.Hash()) != nil {
		t.Fatal("the transfer of the old branch is still found on the canonical chain")
	}
	if !bc.pool.Has(tx.Hash()) {
		t.Fatal("the transfer of the old branch is not back in the txpool (pass the dropped transactions to setHead)")
	}
	st, _, err := bc.HeadState()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.GetBalance(labBob).ToBig(); got.Cmp(labEther(1_000)) != 0 {
		t.Fatalf("bob's balance at the new head = %s, want the genesis balance (the transfer is not on this branch)", got)
	}
	// Mining on the new head picks the transfer up again.
	b3 := lb.next(b[1], 2, labMinerB, tx)
	labMustInsert(t, bc, b3, StatusCanon)
	if info := bc.LookupTx(tx.Hash()); info == nil || info.Block.Hash() != b3.Hash() {
		t.Fatal("after it was mined again, the transfer is not found in its new block")
	}
	if bc.pool.Has(tx.Hash()) {
		t.Fatal("the transfer is still in the txpool after being mined again")
	}
}
