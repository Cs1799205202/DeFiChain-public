package chain

// Tests for task 3 (process.go). Run: go test ./chain -run TestExec -v

import (
	"crypto/ecdsa"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"defichain/client"
)

const labGwei = 1_000_000_000

// labExecChain is a chain without txpool for the execution tests. They call BuildBlock and
// processBlock directly: no PoW, no fork choice.
func labExecChain(t *testing.T) *BlockChain {
	t.Helper()
	bc, err := NewBlockChain(labGenesis(), nil, labLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return bc
}

// labExecute builds a child of parent with txs (mined by coinbase, 10 s later), processes
// it and stores it, so that it can be the parent of the next one. want is the number of
// transactions the block must keep (the valid ones).
func labExecute(t *testing.T, bc *BlockChain, parent *types.Block, coinbase common.Address, want int,
	txs ...*types.Transaction) (*types.Block, *BlockResult) {
	t.Helper()
	h := &types.Header{Coinbase: coinbase, Time: parent.Time() + 10, Difficulty: big.NewInt(4096)}
	b, err := bc.BuildBlock(parent, h, txs)
	if err != nil || b == nil {
		t.Fatalf("BuildBlock: block %v, error %v", b, err)
	}
	if len(b.Transactions()) != want {
		t.Fatalf("BuildBlock kept %d of the %d transactions, want %d", len(b.Transactions()), len(txs), want)
	}
	res, err := bc.processBlock(b)
	if err != nil {
		t.Fatalf("processBlock rejects the block that BuildBlock made: %v", err)
	}
	if res == nil || len(res.Receipts) != want || len(res.Reverts) != want {
		t.Fatalf("processBlock returned %+v, want %d receipts and %d revert reasons", res, want, want)
	}
	bc.writeBlock(b, new(big.Int).Add(bc.GetTd(parent.Hash()), b.Difficulty()), res)
	return b, res
}

// labBalance is addr's balance after block b.
func labBalance(t *testing.T, bc *BlockChain, b *types.Block, addr common.Address) *big.Int {
	t.Helper()
	st, err := bc.StateAt(b)
	if err != nil {
		t.Fatalf("no state after block %d (processBlock must commit it): %v", b.NumberU64(), err)
	}
	return st.GetBalance(addr).ToBig()
}

// Every node must compute exactly the same state root. Block 1 of labGenesis with these
// three transfers has the root below; the balances are checked first, to tell you what is
// wrong if your root differs.
func TestExecStateRootVector(t *testing.T) {
	bc := labExecChain(t)
	txs := []*types.Transaction{
		labTransfer(labAliceKey, 0, labBob, labEther(10), 2*labGwei),
		labTransfer(labBobKey, 0, labCarol, labEther(5), 1*labGwei),
		labTransfer(labAliceKey, 1, labCarol, labEther(1), 3*labGwei),
	}
	b, res := labExecute(t, bc, bc.Genesis(), labMinerA, 3, txs...)
	if b.GasUsed() != 3*21000 {
		t.Errorf("GasUsed = %d, want 63000 (three plain transfers)", b.GasUsed())
	}
	for i, r := range res.Receipts {
		if r.Status != types.ReceiptStatusSuccessful || r.CumulativeGasUsed != uint64(i+1)*21000 {
			t.Errorf("receipt %d: status %d, cumulative gas %d, want 1 and %d", i, r.Status, r.CumulativeGasUsed, (i+1)*21000)
		}
	}
	gwei := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(labGwei)) }
	fee := func(tips ...int64) *big.Int { // 21000 gas per transfer, base fee 0: the fee is all tip
		sum := new(big.Int)
		for _, tip := range tips {
			sum.Add(sum, new(big.Int).Mul(gwei(tip), big.NewInt(21000)))
		}
		return sum
	}
	sub := func(a *big.Int, bs ...*big.Int) *big.Int {
		a = new(big.Int).Set(a)
		for _, b := range bs {
			a.Sub(a, b)
		}
		return a
	}
	add := func(a *big.Int, bs ...*big.Int) *big.Int {
		a = new(big.Int).Set(a)
		for _, b := range bs {
			a.Add(a, b)
		}
		return a
	}
	want := []struct {
		who  string
		addr common.Address
		bal  *big.Int
	}{
		{"the miner (block reward 2 ETH + all fees)", labMinerA, add(labEther(2), fee(2, 1, 3))},
		{"alice", labAlice, sub(labEther(1000), labEther(10), labEther(1), fee(2, 3))},
		{"bob", labBob, sub(add(labEther(1000), labEther(10)), labEther(5), fee(1))},
		{"carol", labCarol, add(labEther(1000), labEther(5), labEther(1))},
	}
	for _, w := range want {
		if got := labBalance(t, bc, b, w.addr); got.Cmp(w.bal) != 0 {
			t.Errorf("balance of %s = %s, want %s", w.who, got, w.bal)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if want := common.HexToHash("0x44c33873bf842924e79f689874bb7b49d417225b899c5b489d95a62acea267cc"); b.Root() != want {
		t.Errorf("state root %s, want %s (balances are right: are the nonces? did you run anything else?)", b.Root(), want)
	}
	if want := common.HexToHash("0x25e6b7af647c519a27cc13276a1e6abc46154b51414d174b072698df1f6c19df"); b.ReceiptHash() != want {
		t.Errorf("receipt root %s, want %s (receipts in block order, with cumulative gas?)", b.ReceiptHash(), want)
	}
}

// labCall signs a contract call (to == nil: a deployment) with tip 1 gwei.
func labCall(key *ecdsa.PrivateKey, nonce uint64, to *common.Address, data []byte) *types.Transaction {
	return types.MustSignNewTx(key, labSigner, &types.DynamicFeeTx{ChainID: big.NewInt(4242), Nonce: nonce,
		GasTipCap: big.NewInt(labGwei), GasFeeCap: big.NewInt(labGwei), Gas: 3_000_000, To: to, Data: data})
}

func labData(t *testing.T, con client.Contract, method string, args ...any) []byte {
	t.Helper()
	var data []byte
	var err error
	if method == "" {
		data, err = con.DeployData(args...)
	} else {
		data, err = con.Calldata(method, args...)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Invalid transactions are left out of a built block; a reverted one stays in it, with a
// failed receipt. Receipts carry the block hash, indexes and logs; the coinbase gets the
// reward and the fees.
func TestExecBuildAndProcess(t *testing.T) {
	bc := labExecChain(t)
	tok := crypto.CreateAddress(labDeployer, 0)
	nobody := client.KeyFor("nobody") // no balance
	deploy := labCall(labDeployerKey, 0, nil, labData(t, client.TokenContract, "", "Test", "TST"))
	pay := labTransfer(labAliceKey, 0, labBob, labEther(1), labGwei)
	gap := labTransfer(labCarolKey, 1, labBob, labEther(1), labGwei) // carol's nonce is 0
	pay2 := labTransfer(labBobKey, 0, labCarol, labEther(1), labGwei)
	poor := labTransfer(nobody, 0, labBob, big.NewInt(1), labGwei)
	b1, res1 := labExecute(t, bc, bc.Genesis(), labMinerA, 3, deploy, pay, gap, pay2, poor)
	for i, want := range []*types.Transaction{deploy, pay, pay2} {
		if got := b1.Transactions()[i]; got.Hash() != want.Hash() {
			t.Fatalf("block 1 transaction %d is %s, want %s (the valid ones, in order)", i, got.Hash().TerminalString(),
				want.Hash().TerminalString())
		}
	}
	if r := res1.Receipts[0]; r.Status != types.ReceiptStatusSuccessful || r.ContractAddress != tok {
		t.Fatalf("deployment receipt: status %d, contract %s, want 1 and %s", r.Status, r.ContractAddress, tok)
	}

	mint := labCall(labDeployerKey, 1, &tok, labData(t, client.TokenContract, "mint", labAlice, big.NewInt(100)))
	broke := labCall(labBobKey, 1, &tok, labData(t, client.TokenContract, "transfer", labCarol, big.NewInt(1))) // bob has no tokens
	send := labCall(labAliceKey, 1, &tok, labData(t, client.TokenContract, "transfer", labBob, big.NewInt(10)))
	b2, res2 := labExecute(t, bc, b1, labMinerB, 3, mint, broke, send)
	if s := []uint64{res2.Receipts[0].Status, res2.Receipts[1].Status, res2.Receipts[2].Status}; s[0] != 1 || s[1] != 0 || s[2] != 1 {
		t.Fatalf("block 2 receipt statuses %v, want [1 0 1]: a reverted transaction stays in the block, with Status 0", s)
	}
	if res2.Reverts[0] != "" || res2.Reverts[1] != "balance" || res2.Reverts[2] != "" {
		t.Fatalf("block 2 revert reasons %q, want [\"\" \"balance\" \"\"]", res2.Reverts)
	}
	for _, c := range []struct {
		b   *types.Block
		res *BlockResult
	}{{b1, res1}, {b2, res2}} {
		var cumulative uint64
		logIndex := uint(0)
		for i, r := range c.res.Receipts {
			if r.TxHash != c.b.Transactions()[i].Hash() || r.TransactionIndex != uint(i) || r.BlockHash != c.b.Hash() ||
				r.BlockNumber.Uint64() != c.b.NumberU64() {
				t.Fatalf("block %d receipt %d: tx %s index %d block %s %v: want this block's hash, number and order",
					c.b.NumberU64(), i, r.TxHash.TerminalString(), r.TransactionIndex, r.BlockHash.TerminalString(), r.BlockNumber)
			}
			if r.CumulativeGasUsed <= cumulative {
				t.Fatalf("block %d receipt %d: cumulative gas %d after %d", c.b.NumberU64(), i, r.CumulativeGasUsed, cumulative)
			}
			cumulative = r.CumulativeGasUsed
			for _, l := range r.Logs {
				if l.BlockHash != c.b.Hash() || l.TxIndex != uint(i) || l.Index != logIndex {
					t.Fatalf("block %d receipt %d: log %+v, want block hash, tx index %d, log index %d", c.b.NumberU64(), i, l, i, logIndex)
				}
				logIndex++
			}
		}
		if cumulative != c.b.GasUsed() {
			t.Fatalf("block %d: last cumulative gas %d, GasUsed %d", c.b.NumberU64(), cumulative, c.b.GasUsed())
		}
	}
	if len(res2.Receipts[0].Logs) != 1 || len(res2.Receipts[1].Logs) != 0 || len(res2.Receipts[2].Logs) != 1 {
		t.Fatal("block 2: want one Transfer log for the mint and one for the transfer, none for the revert")
	}
	if !types.BloomLookup(b2.Bloom(), tok) {
		t.Fatal("block 2's Bloom does not contain the token address")
	}
	reward := labEther(2)
	for _, c := range []struct {
		b     *types.Block
		miner common.Address
	}{{b1, labMinerA}, {b2, labMinerB}} {
		want := new(big.Int).Add(reward, new(big.Int).Mul(new(big.Int).SetUint64(c.b.GasUsed()), big.NewInt(labGwei)))
		if got := labBalance(t, bc, b2, c.miner); got.Cmp(want) != 0 {
			t.Fatalf("miner of block %d has %s, want reward + fees = %s", c.b.NumberU64(), got, want)
		}
	}
	st, _ := bc.StateAt(b2)
	ret, err := bc.Executor().Call(st, b2.Header(), bc.GetHashFn(b2.Header()),
		CallMsg{From: labBob, To: &tok, Data: labData(t, client.TokenContract, "balanceOf", labBob)})
	if err != nil || ret.Failed() || new(big.Int).SetBytes(ret.ReturnData).Int64() != 10 {
		t.Fatalf("bob's token balance after block 2: %v %v, want 10", ret, err)
	}
}

// processBlock rejects a block whose header does not match its execution, or whose body
// does not match its header, with an error wrapping ErrInvalidBlock.
func TestExecRejectsInvalidBlocks(t *testing.T) {
	bc := labExecChain(t)
	g := bc.Genesis()
	pay := labTransfer(labAliceKey, 0, labBob, labEther(1), labGwei)
	gap := labTransfer(labCarolKey, 1, labBob, labEther(1), labGwei) // invalid: carol's nonce is 0
	h := &types.Header{Coinbase: labMinerA, Time: g.Time() + 10, Difficulty: big.NewInt(4096)}
	b, err := bc.BuildBlock(g, h, []*types.Transaction{pay, gap})
	if err != nil || b == nil || len(b.Transactions()) != 1 {
		t.Fatalf("BuildBlock: %v %v (want a block with the valid transaction only)", b, err)
	}
	check := func(name string, bad *types.Block) {
		t.Helper()
		var err error
		labNoPanic(t, "processBlock("+name+")", func() { _, err = bc.processBlock(bad) })
		if !errors.Is(err, ErrInvalidBlock) {
			t.Errorf("%s: processBlock = %v, want an error wrapping ErrInvalidBlock", name, err)
		}
	}
	tamper := func(f func(*types.Header)) *types.Block {
		h := b.Header()
		f(h)
		return b.WithSeal(h)
	}
	check("wrong state root", tamper(func(h *types.Header) { h.Root[0] ^= 1 }))
	check("wrong gas used", tamper(func(h *types.Header) { h.GasUsed++ }))
	check("wrong receipt root", tamper(func(h *types.Header) { h.ReceiptHash[0] ^= 1 }))
	check("wrong logs bloom", tamper(func(h *types.Header) { h.Bloom[0] ^= 1 }))
	check("transactions do not match TxHash", tamper(func(h *types.Header) { h.TxHash[0] ^= 1 }))
	// The invalid transaction is in the body, and TxHash covers it; every other field is what
	// the block would have without it. Executing strictly is the only way to see the problem.
	forged := tamper(func(h *types.Header) { h.TxHash = txsRoot([]*types.Transaction{pay, gap}) }).
		WithBody(types.Body{Transactions: []*types.Transaction{pay, gap}})
	check("an invalid transaction in the block", forged)
	uncle := types.CopyHeader(g.Header())
	check("an uncle in the body", b.WithBody(types.Body{Transactions: b.Transactions(), Uncles: []*types.Header{uncle}}))

	if _, err := bc.processBlock(b); err != nil {
		t.Fatalf("processBlock(the valid block) after the invalid ones = %v, want nil", err)
	}
}

// labNoPanic runs f and turns a panic into a test failure.
func labNoPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	f()
}
