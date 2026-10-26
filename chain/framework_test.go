package chain

// Framework tests: the EVM (one transaction at a time, through applyTx), the contracts and
// the chain store. They do not use any of the files you write, so they pass from the start;
// if one fails, the framework is broken (or you changed a framework file), not your code.

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"

	"defichain/client"
	"defichain/spec"
)

// labExecGenesis funds labDeployer, labAlice and labBob.
func labExecGenesis() *spec.Genesis {
	cfg := spec.DefaultChainConfig()
	cfg.DifficultyBoundDivisor, cfg.MinimumDifficulty, cfg.MaxFutureTime = 2048, 2048, 3600
	g := &spec.Genesis{Config: cfg, Timestamp: 1_700_000_000, Difficulty: 1 << 14, Alloc: map[string]spec.Account{}}
	for _, a := range []common.Address{labDeployer, labAlice, labBob} {
		g.Alloc[a.Hex()] = spec.Account{Balance: spec.NewBig(labEther(1_000))}
	}
	return g
}

// evm executes one transaction per block with the framework's executor, without PoW.
type labEVM struct {
	t       *testing.T
	x       *Executor
	headers map[common.Hash]*types.Header
	head    *types.Header
	nonces  map[common.Address]uint64
}

func labNewEVM(t *testing.T) *labEVM {
	t.Helper()
	db := newStateDatabase()
	gen, err := GenesisBlock(labExecGenesis(), db)
	if err != nil {
		t.Fatal(err)
	}
	e := &labEVM{t: t, x: newExecutor(labExecGenesis().Config, db), headers: map[common.Hash]*types.Header{}, nonces: map[common.Address]uint64{}}
	e.head = gen.Header()
	e.headers[gen.Hash()] = e.head
	return e
}

func (e *labEVM) getHash(h *types.Header) func(uint64) common.Hash {
	return func(n uint64) common.Hash {
		for p := e.headers[h.ParentHash]; p != nil; p = e.headers[p.ParentHash] {
			if p.Number.Uint64() == n {
				return p.Hash()
			}
		}
		return common.Hash{}
	}
}

func (e *labEVM) sign(key *ecdsa.PrivateKey, to *common.Address, data []byte, value *big.Int) *types.Transaction {
	from := client.AddressOf(key)
	if value == nil {
		value = new(big.Int)
	}
	tx := types.MustSignNewTx(key, e.x.signer, &types.DynamicFeeTx{ChainID: big.NewInt(4242), Nonce: e.nonces[from],
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 5_000_000, To: to, Value: value, Data: data})
	e.nonces[from]++
	return tx
}

// block executes txs one after the other in a new block, 12 s after the head, with the
// framework's applyTx only (no block reward, no header commitments: that is task 3), and
// makes the result the new head.
func (e *labEVM) block(txs ...*types.Transaction) *BlockResult {
	e.t.Helper()
	h := &types.Header{ParentHash: e.head.Hash(), Number: new(big.Int).Add(e.head.Number, common.Big1), Time: e.head.Time + 12,
		Coinbase: labMinerA, Difficulty: big.NewInt(1 << 14), GasLimit: e.x.cfg.GasLimit, BaseFee: new(big.Int)}
	st := e.state()
	gp := newGasPool(h)
	res := &BlockResult{}
	for i, tx := range txs {
		receipt, reason, err := e.x.applyTx(h, common.Hash{}, e.getHash(h), st, gp, i, tx)
		if err != nil {
			e.t.Fatalf("transaction %d is invalid: %v", i, err)
		}
		res.Receipts = append(res.Receipts, receipt)
		res.Reverts = append(res.Reverts, reason)
	}
	h.Root = e.x.stateRoot(h, st)
	if err := e.x.commitState(h, st); err != nil {
		e.t.Fatal(err)
	}
	e.head = h
	e.headers[h.Hash()] = h
	return res
}

// send executes one transaction; wantRevert is the expected revert reason ("" = success).
func (e *labEVM) send(key *ecdsa.PrivateKey, to *common.Address, data []byte, wantRevert string) *types.Receipt {
	e.t.Helper()
	res := e.block(e.sign(key, to, data, nil))
	if res.Reverts[0] != wantRevert {
		e.t.Fatalf("revert reason %q, want %q", res.Reverts[0], wantRevert)
	}
	return res.Receipts[0]
}

func (e *labEVM) deploy(con client.Contract, args ...any) common.Address {
	e.t.Helper()
	data, err := con.DeployData(args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.send(labDeployerKey, nil, data, "").ContractAddress
}

func (e *labEVM) call(key *ecdsa.PrivateKey, con client.Contract, addr common.Address, revert, method string, args ...any) {
	e.t.Helper()
	data, err := con.Calldata(method, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	e.send(key, &addr, data, revert)
}

func (e *labEVM) state() *state.StateDB {
	st, err := state.New(e.head.Root, e.x.db)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *labEVM) view(con client.Contract, addr common.Address, method string, args ...any) *big.Int {
	e.t.Helper()
	data, _ := con.Calldata(method, args...)
	res, err := e.x.Call(e.state(), e.head, e.getHash(e.head), CallMsg{From: labDeployer, To: &addr, Data: data})
	if err != nil || res.Failed() {
		e.t.Fatalf("view %s: %v %v", method, err, res)
	}
	out, err := con.Decode(method, res.ReturnData)
	if err != nil {
		e.t.Fatal(err)
	}
	return out[0].(*big.Int)
}

func TestFrameworkTokenAMMLending(t *testing.T) {
	e := labNewEVM(t)
	usd := e.deploy(client.TokenContract, "US Dollar", "USD")
	weth := e.deploy(client.TokenContract, "Wrapped Ether", "WETH")
	for _, who := range []common.Address{labDeployer, labAlice, labBob} {
		e.call(labDeployerKey, client.TokenContract, usd, "", "mint", who, labEther(1_000_000))
		e.call(labDeployerKey, client.TokenContract, weth, "", "mint", who, labEther(1_000))
	}
	pool := e.deploy(client.AMMContract, weth, usd)
	maxU := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	for _, key := range []*ecdsa.PrivateKey{labDeployerKey, labAliceKey, labBobKey} {
		e.call(key, client.TokenContract, usd, "", "approve", pool, maxU)
		e.call(key, client.TokenContract, weth, "", "approve", pool, maxU)
	}
	e.call(labDeployerKey, client.AMMContract, pool, "", "addLiquidity", labEther(100), labEther(200_000))
	if p := e.view(client.AMMContract, pool, "spotPrice0"); p.Cmp(labEther(2000)) != 0 {
		t.Fatalf("spot price %s", p)
	}
	before := e.view(client.TokenContract, usd, "balanceOf", labAlice)
	e.call(labAliceKey, client.AMMContract, pool, "", "swap", weth, labEther(10), big.NewInt(0))
	got := new(big.Int).Sub(e.view(client.TokenContract, usd, "balanceOf", labAlice), before)
	if got.Cmp(labEther(18_132)) < 0 || got.Cmp(labEther(18_133)) > 0 {
		t.Fatalf("swap out %s, want ~18132e18", got)
	}
	e.call(labAliceKey, client.AMMContract, pool, "slippage", "swap", weth, labEther(1), labEther(1_000_000))

	oracle := e.deploy(client.FeedOracleContract, labEther(2000))
	lending := e.deploy(client.LendingContract, weth, usd, oracle, big.NewInt(0))
	e.call(labDeployerKey, client.TokenContract, usd, "", "transfer", lending, labEther(500_000))
	e.call(labBobKey, client.TokenContract, weth, "", "approve", lending, maxU)
	e.call(labBobKey, client.LendingContract, lending, "", "deposit", labEther(10))
	e.call(labBobKey, client.LendingContract, lending, "exceeds LTV", "borrow", labEther(15_500))
	e.call(labBobKey, client.LendingContract, lending, "", "borrow", labEther(14_000))
	e.call(labAliceKey, client.TokenContract, usd, "", "approve", lending, maxU)
	e.call(labAliceKey, client.LendingContract, lending, "healthy", "liquidate", labBob, labEther(1_000))
	e.call(labDeployerKey, client.FeedOracleContract, oracle, "", "setPrice", labEther(1500))
	wethBefore := e.view(client.TokenContract, weth, "balanceOf", labAlice)
	e.call(labAliceKey, client.LendingContract, lending, "", "liquidate", labBob, labEther(7_000))
	seized := new(big.Int).Sub(e.view(client.TokenContract, weth, "balanceOf", labAlice), wethBefore)
	if want := new(big.Int).Div(labEther(49), big.NewInt(10)); seized.Cmp(want) != 0 { // 7000*1.05/1500
		t.Fatalf("seized %s, want %s", seized, want)
	}
}

// When the collateral cannot cover repay*(1+bonus), the liquidator takes all of it and pays
// only its worth (regression: it used to pay the full requested amount).
func TestFrameworkLiquidationCappedByCollateral(t *testing.T) {
	e := labNewEVM(t)
	usd := e.deploy(client.TokenContract, "US Dollar", "USD")
	weth := e.deploy(client.TokenContract, "Wrapped Ether", "WETH")
	oracle := e.deploy(client.FeedOracleContract, labEther(2000))
	lending := e.deploy(client.LendingContract, weth, usd, oracle, big.NewInt(0))
	maxU := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	e.call(labDeployerKey, client.TokenContract, usd, "", "mint", lending, labEther(100_000))
	e.call(labDeployerKey, client.TokenContract, usd, "", "mint", labAlice, labEther(100_000))
	e.call(labDeployerKey, client.TokenContract, weth, "", "mint", labBob, labEther(10))
	e.call(labBobKey, client.TokenContract, weth, "", "approve", lending, maxU)
	e.call(labAliceKey, client.TokenContract, usd, "", "approve", lending, maxU)
	e.call(labBobKey, client.LendingContract, lending, "", "deposit", labEther(10))
	e.call(labBobKey, client.LendingContract, lending, "", "borrow", labEther(15_000))
	e.call(labDeployerKey, client.FeedOracleContract, oracle, "", "setPrice", labEther(1000))
	e.call(labAliceKey, client.LendingContract, lending, "", "liquidate", labBob, labEther(7_500))
	left := e.view(client.LendingContract, lending, "collateralOf", labBob)
	usdBefore := e.view(client.TokenContract, usd, "balanceOf", labAlice)
	e.call(labAliceKey, client.LendingContract, lending, "", "liquidate", labBob, labEther(3_750))
	paid := new(big.Int).Sub(usdBefore, e.view(client.TokenContract, usd, "balanceOf", labAlice))
	if e.view(client.LendingContract, lending, "collateralOf", labBob).Sign() != 0 {
		t.Fatal("collateral should be exhausted")
	}
	want := new(big.Int).Div(new(big.Int).Mul(left, big.NewInt(1000*10000)), big.NewInt(10500))
	if d := new(big.Int).Sub(paid, want); d.CmpAbs(big.NewInt(1e6)) > 0 {
		t.Fatalf("paid %s, want %s", paid, want)
	}
}

func TestFrameworkInterestAccruesWithBlockTime(t *testing.T) {
	e := labNewEVM(t)
	usd := e.deploy(client.TokenContract, "US Dollar", "USD")
	weth := e.deploy(client.TokenContract, "Wrapped Ether", "WETH")
	oracle := e.deploy(client.FeedOracleContract, labEther(2000))
	lending := e.deploy(client.LendingContract, weth, usd, oracle, big.NewInt(1e12))
	maxU := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	e.call(labDeployerKey, client.TokenContract, usd, "", "mint", lending, labEther(100_000))
	e.call(labDeployerKey, client.TokenContract, weth, "", "mint", labBob, labEther(10))
	e.call(labBobKey, client.TokenContract, weth, "", "approve", lending, maxU)
	e.call(labBobKey, client.LendingContract, lending, "", "deposit", labEther(10))
	e.call(labBobKey, client.LendingContract, lending, "", "borrow", labEther(1_000))
	start := e.view(client.LendingContract, lending, "debtOf", labBob)
	for i := 0; i < 5; i++ {
		e.block() // 12 s each
	}
	if end := e.view(client.LendingContract, lending, "debtOf", labBob); end.Cmp(start) <= 0 {
		t.Fatalf("debt did not grow: %s -> %s", start, end)
	}
}

// The chain store: the transaction index follows the canonical chain, setHead records
// reorgs, and BLOCKHASH sees the branch of the block that runs. The blocks here are not
// executed (labStateBlock), so this needs none of your files.
func TestFrameworkSetHeadRecordsReorgs(t *testing.T) {
	bc, err := NewBlockChain(labExecGenesis(), nil, labLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	g := bc.Genesis()
	tx := labTransfer(labAliceKey, 0, labBob, big.NewInt(1), 1)
	a1 := labStateBlock(t, bc, g, labMinerA, nil, tx)
	labSetHead(bc, a1)
	if info := bc.LookupTx(tx.Hash()); info == nil || info.Block.Hash() != a1.Hash() || info.Index != 0 {
		t.Fatalf("lookup %+v", info)
	}
	// Competing branch from genesis: b1, b2 (no tx). Make it canonical by hand.
	b1 := labStateBlock(t, bc, g, labBob, nil)
	b2 := labStateBlock(t, bc, b1, labBob, nil)
	bc.writeCanonical(b1)
	labSetHead(bc, b2, a1.Transactions()...)
	if bc.LookupTx(tx.Hash()) != nil {
		t.Fatal("tx of the old branch still indexed")
	}
	if bc.CanonicalHash(1) != b1.Hash() || bc.CurrentBlock().Hash() != b2.Hash() {
		t.Fatal("canonical chain is not genesis-b1-b2")
	}
	evs, n := bc.Reorgs()
	if n != 1 || evs[0].Removed != 1 || evs[0].Added != 2 || evs[0].Ancestor.Number != 0 || evs[0].DroppedTxs != 1 {
		t.Fatalf("reorg events %+v", evs)
	}
	if got := bc.GetHashFn(b2.Header())(1); got != b1.Hash() {
		t.Fatalf("BLOCKHASH(1) on the new branch = %s, want b1", got.TerminalString())
	}
}
