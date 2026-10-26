package chain

// Test fixtures shared by the chain tests: framework_test.go, the tests of your tasks
// (pow_test.go, consensus_test.go, process_test.go, txpool_test.go, blockchain_test.go,
// miner_test.go, sync_test.go) and the hidden tests used for grading. Every identifier here starts with "lab": do not use that
// prefix in your own code, so that the test files always compile next to it.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"

	"defichain/client"
	"defichain/spec"
)

var (
	labDeployerKey = client.KeyFor("deployer")
	labAliceKey    = client.KeyFor("alice")
	labBobKey      = client.KeyFor("bob")
	labCarolKey    = client.KeyFor("carol")
	labDeployer    = client.AddressOf(labDeployerKey)
	labAlice       = client.AddressOf(labAliceKey)
	labBob         = client.AddressOf(labBobKey)
	labCarol       = client.AddressOf(labCarolKey)
	labMinerA      = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	labMinerB      = common.HexToAddress("0x00000000000000000000000000000000000000bb")
)

const labGenesisTime = 1_700_000_000

// labNow is the clock of the chain tests: long after every block they build, so that no
// block is "in the future" unless a test sets the clock itself.
var labNow = time.Unix(labGenesisTime+1_000_000, 0)

func labEther(n int64) *big.Int { return client.Ether(n) }

// labGenesis: 2 s target, divisor 16 (difficulty reacts fast, so forks of different
// weight are easy to build), small difficulty (blocks seal in microseconds), and four
// funded accounts.
func labGenesis() *spec.Genesis {
	cfg := spec.DefaultChainConfig()
	cfg.TargetBlockTime = 2
	cfg.DifficultyBoundDivisor = 16
	cfg.MinimumDifficulty = 1024
	cfg.MaxFutureTime = 15
	g := &spec.Genesis{Config: cfg, Timestamp: labGenesisTime, Difficulty: 4096, Alloc: map[string]spec.Account{}}
	for _, a := range []common.Address{labDeployer, labAlice, labBob, labCarol} {
		g.Alloc[a.Hex()] = spec.Account{Balance: spec.NewBig(labEther(1_000))}
	}
	return g
}

// labLog keeps a test's log output and shows it only if the test fails.
type labLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *labLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() < 1<<20 {
		l.buf.Write(p)
	}
	return len(p), nil
}

func labLogger(t *testing.T) *log.Logger {
	l := &labLog{}
	t.Cleanup(func() {
		if t.Failed() {
			l.mu.Lock()
			if l.buf.Len() > 0 {
				t.Logf("log output:\n%s", l.buf.String())
			}
			l.mu.Unlock()
		}
	})
	return log.New(l, "", log.Lmicroseconds)
}

// labChain is a fresh chain under test, with a txpool, whose clock reads labNow.
func labChain(t *testing.T) *BlockChain {
	t.Helper()
	bc, err := NewBlockChain(labGenesis(), NewTxPool(), labLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	bc.SetClock(func() time.Time { return labNow })
	return bc
}

// labDo runs f and fails the test if it does not return within d.
func labDo(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: a deadlock? (For example, insertBlock runs with bc.insertMu held, "+
			"so it must not call InsertBlock or InsertChain.)", what, d)
	}
}

// labInsert is bc.InsertBlock(b) with a deadlock check.
func labInsert(t *testing.T, bc *BlockChain, b *types.Block) (InsertStatus, error) {
	t.Helper()
	var status InsertStatus
	var err error
	labDo(t, 20*time.Second, "InsertBlock", func() { status, err = bc.InsertBlock(b) })
	return status, err
}

// labMustInsert inserts b and checks that it succeeds with status want.
func labMustInsert(t *testing.T, bc *BlockChain, b *types.Block, want InsertStatus) {
	t.Helper()
	status, err := labInsert(t, bc, b)
	if err != nil {
		t.Fatalf("InsertBlock(block %d %s): unexpected error: %v", b.NumberU64(), b.Hash().TerminalString(), err)
	}
	if status != want {
		t.Fatalf("InsertBlock(block %d %s) = %s, want %s", b.NumberU64(), b.Hash().TerminalString(), status, want)
	}
}

// labWantCanonical checks that tip is the head and that the canonical index is exactly the
// path from genesis to tip: every height up to tip maps to tip's ancestor at that height,
// and the heights above tip map to nothing.
func labWantCanonical(t *testing.T, bc *BlockChain, tip *types.Block) {
	t.Helper()
	if head := bc.CurrentBlock(); head.Hash() != tip.Hash() {
		t.Fatalf("head is block %d %s, want block %d %s", head.NumberU64(), head.Hash().TerminalString(),
			tip.NumberU64(), tip.Hash().TerminalString())
	}
	for b := tip; b != nil; b = bc.GetBlock(b.ParentHash()) {
		if got := bc.CanonicalHash(b.NumberU64()); got != b.Hash() {
			t.Fatalf("canonical block %d is %s, want %s (an ancestor of the head)", b.NumberU64(), got.TerminalString(),
				b.Hash().TerminalString())
		}
		if b.NumberU64() == 0 {
			break
		}
	}
	for n := tip.NumberU64() + 1; n <= tip.NumberU64()+64; n++ {
		if got := bc.CanonicalHash(n); got != (common.Hash{}) {
			t.Fatalf("height %d (above the head, %d) is still canonical: %s; deleteCanonical it during the reorg",
				n, tip.NumberU64(), got.TerminalString())
		}
	}
}

// ---- transactions and state without executing blocks ----------------------------------

var labSigner = types.LatestSignerForChainID(big.NewInt(4242))

// labTransfer is a signed EIP-1559 transfer on chain 4242 with the given tip (= fee cap).
func labTransfer(key *ecdsa.PrivateKey, nonce uint64, to common.Address, value *big.Int, tip int64) *types.Transaction {
	return types.MustSignNewTx(key, labSigner, &types.DynamicFeeTx{ChainID: big.NewInt(4242), Nonce: nonce,
		GasTipCap: big.NewInt(tip), GasFeeCap: big.NewInt(tip), Gas: 21000, To: &to, Value: value})
}

// labStateBlock makes a child of parent whose state is parent's state changed by edit
// (for example, nonces set by hand), and stores it in bc: a block without execution, for
// the tests that need a state but not a valid block (txpool, chain store).
func labStateBlock(t *testing.T, bc *BlockChain, parent *types.Block, coinbase common.Address,
	edit func(*state.StateDB), txs ...*types.Transaction) *types.Block {
	t.Helper()
	st, err := bc.StateAt(parent)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(st)
	}
	h := &types.Header{ParentHash: parent.Hash(), Number: new(big.Int).Add(parent.Number(), common.Big1),
		Time: parent.Time() + 10, Coinbase: coinbase, Difficulty: big.NewInt(4096), GasLimit: bc.cfg.GasLimit,
		BaseFee: new(big.Int)}
	h.Root, err = st.Commit(bc.exec.rules(h), h.Number.Uint64())
	if err != nil {
		t.Fatal(err)
	}
	b := types.NewBlock(h, &types.Body{Transactions: txs}, nil, trie.NewStackTrie(nil))
	bc.writeBlock(b, new(big.Int).Add(bc.GetTd(parent.Hash()), b.Difficulty()), &BlockResult{})
	return b
}

// labSetHead makes b (stored, with its ancestors canonical) the canonical head with the
// framework helpers, as a reorg would: setHead resets the txpool with dropped.
func labSetHead(bc *BlockChain, b *types.Block, dropped ...*types.Transaction) {
	for n := b.NumberU64() + 1; bc.CanonicalHash(n) != (common.Hash{}); n++ {
		bc.deleteCanonical(n)
	}
	bc.writeCanonical(b)
	bc.setHead(b, dropped)
}

// labNonces returns an edit for labStateBlock that sets account nonces.
func labNonces(kv ...any) func(*state.StateDB) {
	return func(st *state.StateDB) {
		for i := 0; i < len(kv); i += 2 {
			st.SetNonce(kv[i].(common.Address), uint64(kv[i+1].(int)), tracing.NonceChangeUnspecified)
		}
	}
}

// ---- building blocks independently of the chain under test -----------------------------

// labBuilder makes valid blocks without using the chain under test: it has its own block
// store, written with the framework helpers only (no fork choice), executes blocks with
// build and process, and seals them with seal.
type labBuilder struct {
	t     *testing.T
	bc    *BlockChain
	seal  func(*types.Block) (*types.Block, error)
	diff  func(spec.ChainConfig, uint64, *types.Header) *big.Int
	check bool // VerifySeal every sealed block (when sealing with your own Seal)
	// build and process execute blocks: your BuildBlock and processBlock (task 3) here;
	// the hidden tests use a reference implementation instead.
	build   func(bc *BlockChain, parent *types.Block, h *types.Header, txs []*types.Transaction) (*types.Block, error)
	process func(bc *BlockChain, b *types.Block) (*BlockResult, error)
}

// labNewBuilder seals with your Seal and CalcDifficulty, and executes with your process.go.
func labNewBuilder(t *testing.T) *labBuilder {
	t.Helper()
	bc, err := NewBlockChain(labGenesis(), nil, labLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return &labBuilder{t: t, bc: bc, check: true, diff: CalcDifficulty, seal: func(b *types.Block) (*types.Block, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return Seal(ctx, b, nil)
	}, build: (*BlockChain).BuildBlock, process: (*BlockChain).processBlock}
}

func (lb *labBuilder) genesis() *types.Block { return lb.bc.Genesis() }

// tx is a transfer of value wei from key to `to` with the given nonce.
func (lb *labBuilder) tx(key *ecdsa.PrivateKey, nonce uint64, to common.Address, value int64) *types.Transaction {
	return types.MustSignNewTx(key, lb.bc.exec.signer, &types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(lb.bc.cfg.ChainID),
		Nonce: nonce, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 21000, To: &to, Value: big.NewInt(value)})
}

// next builds, seals and stores a child of parent, dt seconds after it, mined by coinbase.
func (lb *labBuilder) next(parent *types.Block, dt uint64, coinbase common.Address, txs ...*types.Transaction) *types.Block {
	lb.t.Helper()
	ts := parent.Time() + dt
	d := lb.diff(lb.bc.cfg, ts, parent.Header())
	if d == nil || d.Sign() <= 0 {
		lb.t.Fatalf("CalcDifficulty returned %v: not implemented yet?", d)
	}
	b, err := lb.build(lb.bc, parent, &types.Header{Coinbase: coinbase, Time: ts, Difficulty: d}, txs)
	if err != nil || b == nil {
		lb.t.Fatalf("BuildBlock: block %v, error %v (not implemented yet?)", b, err)
	}
	if len(b.Transactions()) != len(txs) {
		lb.t.Fatalf("test bug: %d of %d transactions are invalid on this branch", len(txs)-len(b.Transactions()), len(txs))
	}
	sealed, err := lb.seal(b)
	if err != nil || sealed == nil {
		lb.t.Fatalf("Seal: block %v, error %v (not implemented yet?)", sealed, err)
	}
	if lb.check {
		if err := VerifySeal(sealed.Header()); err != nil {
			lb.t.Fatalf("the block your Seal returned does not pass your VerifySeal: %v", err)
		}
	}
	res, err := lb.process(lb.bc, sealed)
	if err != nil {
		lb.t.Fatalf("processBlock rejects a block that BuildBlock made: %v", err)
	}
	lb.bc.writeBlock(sealed, new(big.Int).Add(lb.bc.GetTd(parent.Hash()), sealed.Difficulty()), res)
	return sealed
}

// chain builds n blocks on parent, dt seconds apart, and returns them oldest first.
func (lb *labBuilder) chain(parent *types.Block, n int, dt uint64, coinbase common.Address) []*types.Block {
	lb.t.Helper()
	out := make([]*types.Block, 0, n)
	for i := 0; i < n; i++ {
		parent = lb.next(parent, dt, coinbase)
		out = append(out, parent)
	}
	return out
}

// td is the total difficulty of a block the builder made.
func (lb *labBuilder) td(b *types.Block) *big.Int { return lb.bc.GetTd(b.Hash()) }

// ---- a fake network for the sync tests -------------------------------------------------

// labFakeNet is a PeerNet whose peers are labBuilders' block stores.
type labFakeNet struct {
	mu        sync.Mutex
	peers     map[string]*labBuilder
	heads     map[string]*types.Block
	down      map[string]bool // the peer does not answer requests
	lie       map[string]bool // the peer answers RequestAncestors with blocks that do not chain
	broadcast []labSent
	sent      []labSent
	requests  map[string]int // RequestAncestors calls per peer
}

type labSent struct {
	block      *types.Block
	except, to string
}

func labNewFakeNet() *labFakeNet {
	return &labFakeNet{peers: map[string]*labBuilder{}, heads: map[string]*types.Block{}, down: map[string]bool{},
		lie: map[string]bool{}, requests: map[string]int{}}
}

// add makes builder lb a peer called id, whose head is head.
func (f *labFakeNet) add(id string, lb *labBuilder, head *types.Block) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers[id], f.heads[id] = lb, head
}

func (f *labFakeNet) Peers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.peers {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

func (f *labFakeNet) BroadcastBlock(b *types.Block, except string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast = append(f.broadcast, labSent{block: b, except: except})
}

func (f *labFakeNet) SendBlock(peer string, b *types.Block) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, labSent{block: b, to: peer})
}

func (f *labFakeNet) RequestHead(peer string) (PeerHead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, head := f.peers[peer], f.heads[peer]
	if lb == nil || f.down[peer] {
		return PeerHead{}, fmt.Errorf("peer %s does not answer", peer)
	}
	return PeerHead{Number: head.NumberU64(), Hash: head.Hash(), TD: lb.td(head)}, nil
}

func (f *labFakeNet) RequestAncestors(peer string, hash common.Hash, max int) ([]*types.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[peer]++
	lb := f.peers[peer]
	if lb == nil || f.down[peer] {
		return nil, fmt.Errorf("peer %s does not answer", peer)
	}
	b := lb.bc.GetBlock(hash)
	if b == nil {
		return nil, fmt.Errorf("peer %s: unknown block %s", peer, hash.TerminalString())
	}
	if max <= 0 || max > MaxAncestors {
		max = MaxAncestors
	}
	var out []*types.Block
	for b != nil && b.NumberU64() > 0 && len(out) < max {
		out = append(out, b)
		b = lb.bc.GetBlock(b.ParentHash())
	}
	if f.lie[peer] && len(out) > 2 {
		out[1], out[2] = out[2], out[1]
	}
	return out, nil
}

// broadcasts returns what was broadcast, and forgets it.
func (f *labFakeNet) broadcasts() []labSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.broadcast
	f.broadcast = nil
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// labRecorder is a Broadcaster that remembers what the miner announced.
type labRecorder struct {
	mu     sync.Mutex
	blocks []*types.Block
}

func (r *labRecorder) BroadcastBlock(b *types.Block, except string) {
	r.mu.Lock()
	r.blocks = append(r.blocks, b)
	r.mu.Unlock()
}

func (r *labRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.blocks)
}
