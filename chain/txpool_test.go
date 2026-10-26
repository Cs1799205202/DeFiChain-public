package chain

// Tests for task 4 (txpool.go). Run: go test ./chain -run TestTxPool -v

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"

	"defichain/client"
)

// labPoolChain is a chain with a fresh txpool; the tests move its head with labStateBlock
// and labSetHead (no execution, no PoW: only your txpool runs).
func labPoolChain(t *testing.T) (*BlockChain, *TxPool) {
	t.Helper()
	bc, err := NewBlockChain(labGenesis(), NewTxPool(), labLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return bc, bc.pool
}

func labMustAdd(t *testing.T, pool *TxPool, txs ...*types.Transaction) {
	t.Helper()
	for _, tx := range txs {
		var ok bool
		var err error
		labNoPanic(t, "Add", func() { ok, err = pool.Add(tx) })
		if !ok || err != nil {
			t.Fatalf("Add(%s) = %v, %v; want true, nil", tx.Hash().TerminalString(), ok, err)
		}
	}
}

// labWantPending checks that Pending returns exactly want, in this order.
func labWantPending(t *testing.T, pool *TxPool, names map[common.Hash]string, want ...*types.Transaction) {
	t.Helper()
	got := pool.Pending()
	str := func(txs []*types.Transaction) []string {
		var out []string
		for _, tx := range txs {
			out = append(out, names[tx.Hash()])
		}
		return out
	}
	if len(got) != len(want) {
		t.Fatalf("Pending() = %v, want %v", str(got), str(want))
	}
	for i := range want {
		if got[i].Hash() != want[i].Hash() {
			t.Fatalf("Pending() = %v, want %v", str(got), str(want))
		}
	}
}

func TestTxPoolAddAndPending(t *testing.T) {
	_, pool := labPoolChain(t)
	a0, a1, a3 := labTransfer(labAliceKey, 0, labBob, big.NewInt(1), 5), labTransfer(labAliceKey, 1, labBob, big.NewInt(1), 5),
		labTransfer(labAliceKey, 3, labBob, big.NewInt(1), 5) // alice's nonce 2 is missing
	b0, b1 := labTransfer(labBobKey, 0, labCarol, big.NewInt(1), 9), labTransfer(labBobKey, 1, labCarol, big.NewInt(1), 2)
	names := map[common.Hash]string{a0.Hash(): "a0", a1.Hash(): "a1", a3.Hash(): "a3", b0.Hash(): "b0", b1.Hash(): "b1"}
	labMustAdd(t, pool, a0, a1, a3, b0, b1)
	if ok, err := pool.Add(a0); ok || err != nil {
		t.Fatalf("Add(a transaction already in the pool) = %v, %v; want false, nil", ok, err)
	}
	if pool.Len() != 5 || !pool.Has(a3.Hash()) || pool.Get(b1.Hash()) == nil || pool.Get(common.Hash{1}) != nil {
		t.Fatalf("Len = %d, Has(a3) = %v, Get(b1) = %v", pool.Len(), pool.Has(a3.Hash()), pool.Get(b1.Hash()))
	}
	// Highest tip first among the senders' next transactions: b0 (9), then a0 and a1 (5),
	// then b1 (2), which only becomes available after b0. a3 waits for nonce 2.
	labWantPending(t, pool, names, b0, a0, a1, b1)
	if n := pool.PendingNonce(labAlice); n != 2 {
		t.Fatalf("PendingNonce(alice) = %d, want 2 (nonce 0 and 1 are pending; 3 waits for 2)", n)
	}
	if n := pool.PendingNonce(labCarol); n != 0 {
		t.Fatalf("PendingNonce(carol, who has no transaction) = %d, want 0", n)
	}
	pending, queued := pool.Content()
	if len(pending[labAlice]) != 2 || len(queued[labAlice]) != 1 || queued[labAlice][0].Hash() != a3.Hash() ||
		len(pending[labBob]) != 2 || queued[labBob] != nil {
		t.Fatalf("Content(): pending %v, queued %v", pending, queued)
	}
	a2 := labTransfer(labAliceKey, 2, labBob, big.NewInt(1), 5)
	names[a2.Hash()] = "a2"
	labMustAdd(t, pool, a2) // fills the gap: a3 becomes pending too
	labWantPending(t, pool, names, b0, a0, a1, a2, a3, b1)
	if n := pool.PendingNonce(labAlice); n != 4 {
		t.Fatalf("PendingNonce(alice) = %d, want 4", n)
	}
}
func TestTxPoolReplaceAndReject(t *testing.T) {
	bc, pool := labPoolChain(t)
	a0 := labTransfer(labAliceKey, 0, labBob, big.NewInt(1), 5)
	labMustAdd(t, pool, a0)
	reject := func(name string, tx *types.Transaction, want error) {
		t.Helper()
		var ok bool
		var err error
		labNoPanic(t, "Add", func() { ok, err = pool.Add(tx) })
		if ok || !errors.Is(err, want) {
			t.Fatalf("Add(%s) = %v, %v; want false and an error wrapping %v", name, ok, err, want)
		}
	}
	reject("same nonce, same tip", labTransfer(labAliceKey, 0, labBob, big.NewInt(2), 5), ErrReplaceUnderpriced)
	reject("same nonce, lower tip", labTransfer(labAliceKey, 0, labBob, big.NewInt(2), 4), ErrReplaceUnderpriced)
	better := labTransfer(labAliceKey, 0, labBob, big.NewInt(2), 6)
	labMustAdd(t, pool, better)
	if pool.Len() != 1 || pool.Has(a0.Hash()) || !pool.Has(better.Hash()) {
		t.Fatal("a replacement with a higher tip must take the old transaction's place")
	}

	reject("from an account without ether", labTransfer(client.KeyFor("nobody"), 0, labBob, big.NewInt(1), 1),
		core.ErrInsufficientFunds)
	// alice has 1000 ETH: value + gas * fee cap must fit in it.
	reject("value + gas fees above the balance", labTransfer(labAliceKey, 1, labBob, labEther(1000), 1), core.ErrInsufficientFunds)
	reject("unprotected legacy transaction", types.MustSignNewTx(labAliceKey, types.HomesteadSigner{},
		&types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21000, To: &labBob}), ErrUnprotected)
	reject("another chain id", types.MustSignNewTx(labAliceKey, types.LatestSignerForChainID(big.NewInt(1)),
		&types.DynamicFeeTx{ChainID: big.NewInt(1), Nonce: 1, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 21000,
			To: &labBob}), ErrTxChainID)

	// The head moves to a block where alice's nonce is 3 (as if it held her nonces 0..2).
	head := labStateBlock(t, bc, bc.Genesis(), labMinerA, labNonces(labAlice, 3))
	labSetHead(bc, head)
	reject("nonce below the account's nonce", labTransfer(labAliceKey, 2, labBob, big.NewInt(1), 9), core.ErrNonceTooLow)
	if pool.Has(better.Hash()) || pool.Len() != 0 {
		t.Fatal("after the head moved past alice's nonce 0, her nonce-0 transaction must leave the pool (reset)")
	}
	labMustAdd(t, pool, labTransfer(labAliceKey, 3, labBob, big.NewInt(1), 1))
}

// A reorg takes a block off the canonical chain: its transactions come back to the pool,
// unless the new chain has used their nonce.
func TestTxPoolReset(t *testing.T) {
	bc, pool := labPoolChain(t)
	g := bc.Genesis()
	a0, a1 := labTransfer(labAliceKey, 0, labBob, big.NewInt(1), 5), labTransfer(labAliceKey, 1, labBob, big.NewInt(1), 5)
	b0 := labTransfer(labBobKey, 0, labCarol, big.NewInt(1), 5)
	names := map[common.Hash]string{a0.Hash(): "a0", a1.Hash(): "a1", b0.Hash(): "b0"}
	labMustAdd(t, pool, a0, a1, b0)

	// Block x1 holds a0 and b0: they leave the pool.
	x1 := labStateBlock(t, bc, g, labMinerA, labNonces(labAlice, 1, labBob, 1), a0, b0)
	labNoPanic(t, "setHead (reset)", func() { labSetHead(bc, x1) })
	labWantPending(t, pool, names, a1)

	// Reorg to y1, which holds b0 only: a0 is back, b0 is not (y1 used bob's nonce 0).
	// setHead passes what reset returns to onReinject (the node re-broadcasts it).
	var readded []*types.Transaction
	bc.onReinject = func(txs []*types.Transaction) { readded = append(readded, txs...) }
	y1 := labStateBlock(t, bc, g, labMinerB, labNonces(labBob, 1), b0)
	labNoPanic(t, "setHead (reset)", func() { labSetHead(bc, y1, x1.Transactions()...) })
	if len(readded) != 1 || readded[0].Hash() != a0.Hash() {
		t.Fatalf("reset returned %d transactions, want [a0] (b0 is in the new chain too)", len(readded))
	}
	labWantPending(t, pool, names, a0, a1)
	if n := pool.PendingNonce(labAlice); n != 2 {
		t.Fatalf("PendingNonce(alice) = %d, want 2", n)
	}
}
