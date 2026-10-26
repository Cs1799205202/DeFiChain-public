package chain

// Framework file: the EVM, through go-ethereum's library. It executes one transaction at
// a time (applyTx) and serves eth_call / eth_estimateGas; executing and validating whole
// blocks is your task 3, in process.go, built on the helpers below.

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"

	"defichain/spec"
)

// Transaction rules shared by the txpool and block validation.
var (
	ErrTxType      = errors.New("transaction type not supported (only legacy, access-list and EIP-1559)")
	ErrUnprotected = errors.New("legacy transaction without EIP-155 replay protection")
	ErrTxChainID   = errors.New("wrong chain id")
	ErrTxGas       = errors.New("transaction gas exceeds the block gas limit")
)

// ethConfig is go-ethereum's view of our chain: every hard fork up to Prague is active
// from genesis, so contracts compiled by a current solc (whose default target is Prague)
// run unchanged. Later forks are left off on purpose.
func ethConfig(chainID uint64) *params.ChainConfig {
	z := func() *big.Int { return new(big.Int) }
	t0 := func() *uint64 { v := uint64(0); return &v }
	return &params.ChainConfig{
		ChainID:        new(big.Int).SetUint64(chainID),
		HomesteadBlock: z(), EIP150Block: z(), EIP155Block: z(), EIP158Block: z(),
		ByzantiumBlock: z(), ConstantinopleBlock: z(), PetersburgBlock: z(), IstanbulBlock: z(),
		MuirGlacierBlock: z(), BerlinBlock: z(), LondonBlock: z(), ArrowGlacierBlock: z(), GrayGlacierBlock: z(),
		ShanghaiTime: t0(), CancunTime: t0(), PragueTime: t0(),
		TerminalTotalDifficulty: z(),
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: params.DefaultCancunBlobConfig,
			Prague: params.DefaultPragueBlobConfig,
		},
	}
}

// Executor runs transactions on a world state. It is pure: the same parent state, header
// and transactions always give the same result on every node.
type Executor struct {
	cfg    spec.ChainConfig
	eth    *params.ChainConfig
	signer types.Signer
	db     state.Database
}

func newExecutor(cfg spec.ChainConfig, db state.Database) *Executor {
	eth := ethConfig(cfg.ChainID)
	return &Executor{cfg: cfg, eth: eth, signer: types.LatestSignerForChainID(eth.ChainID), db: db}
}

// BlockResult is what executing a block produced, besides the new state.
type BlockResult struct {
	Receipts types.Receipts
	Reverts  []string // per transaction: the revert reason, or "" if it succeeded
}

func (x *Executor) rules(h *types.Header) params.Rules { return x.eth.Rules(h.Number, true, h.Time) }

// CheckTx applies the stateless transaction rules: type, replay protection, chain id,
// signature and gas limit.
func (x *Executor) CheckTx(tx *types.Transaction) (common.Address, error) {
	switch tx.Type() {
	case types.LegacyTxType:
		if !tx.Protected() {
			return common.Address{}, ErrUnprotected
		}
	case types.AccessListTxType, types.DynamicFeeTxType:
	default:
		return common.Address{}, ErrTxType
	}
	if tx.ChainId().Cmp(x.eth.ChainID) != 0 {
		return common.Address{}, fmt.Errorf("%w: %s, this chain is %s", ErrTxChainID, tx.ChainId(), x.eth.ChainID)
	}
	if tx.Gas() > x.cfg.GasLimit {
		return common.Address{}, ErrTxGas
	}
	from, err := types.Sender(x.signer, tx)
	if err != nil {
		return common.Address{}, fmt.Errorf("invalid signature: %w", err)
	}
	return from, nil
}

// ValidateTx is the txpool's admission check for everything that does not depend on the
// state: CheckTx, a fee cap of at least the tip, and enough gas for the intrinsic cost
// (21000 plus calldata, contract creation and access list). Nonce and balance are for the
// pool to check against the head state. It returns the sender.
func (x *Executor) ValidateTx(tx *types.Transaction) (common.Address, error) {
	from, err := x.CheckTx(tx)
	if err != nil {
		return common.Address{}, err
	}
	if tx.GasFeeCapIntCmp(tx.GasTipCap()) < 0 {
		return common.Address{}, core.ErrTipAboveFeeCap
	}
	rules := x.eth.Rules(common.Big0, true, 0) // every fork is active from genesis
	value, _ := uint256.FromBig(tx.Value())
	intrinsic, err := core.IntrinsicGas(tx.Data(), tx.AccessList(), tx.SetCodeAuthorizations(), from, tx.To(), value, rules)
	if err != nil {
		return common.Address{}, err
	}
	if floor, err := core.FloorDataGas(rules, from, tx.To(), value, tx.Data(), tx.AccessList()); err == nil && floor > intrinsic {
		intrinsic = floor
	}
	if tx.Gas() < intrinsic {
		return common.Address{}, fmt.Errorf("%w: have %d, want %d", core.ErrIntrinsicGas, tx.Gas(), intrinsic)
	}
	return from, nil
}

// blockContext is what the EVM sees as "the current block". PREVRANDAO is derived from
// the parent hash, not from the PoW seal, so that a block executes identically before
// and after it is sealed. It is predictable: never use it as randomness in a contract.
func (x *Executor) blockContext(h *types.Header, getHash vm.GetHashFunc) vm.BlockContext {
	random := crypto.Keccak256Hash(h.ParentHash.Bytes())
	return vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     getHash,
		Coinbase:    h.Coinbase,
		BlockNumber: new(big.Int).Set(h.Number),
		Time:        h.Time,
		Difficulty:  new(big.Int).Set(h.Difficulty),
		BaseFee:     new(big.Int),
		BlobBaseFee: new(big.Int),
		GasLimit:    h.GasLimit,
		Random:      &random, // non-nil selects post-merge rules (PUSH0, MCOPY, ...)
	}
}

// ---- helpers for process.go: thin wrappers over go-ethereum's API -----------------------

// openState opens the world state whose root is root (the state after a stored block).
func (x *Executor) openState(root common.Hash) (*state.StateDB, error) {
	st, err := state.New(root, x.db)
	if err != nil {
		return nil, fmt.Errorf("state %s: %w", root.TerminalString(), err)
	}
	return st, nil
}

// newGasPool is the gas budget of a block: every transaction takes its gas out of it, and
// a transaction that does not fit any more is invalid in that block.
func newGasPool(h *types.Header) *core.GasPool { return core.NewGasPool(h.GasLimit) }

// applyTx executes tx, the index-th transaction of block h, on statedb, taking its gas
// out of gp. blockHash goes into the receipt and its logs: the block's hash when
// validating, zero while building (the hash is not known before the seal).
//
// An error means tx is invalid here (bad signature or chain id, nonce not the account's
// next one, cannot pay for its gas, does not fit in gp, ...): it cannot be in this block,
// and applyTx has left statedb and gp as they were. A transaction that runs but reverts
// or runs out of gas is valid: it is in the block, its sender pays for the gas, and its
// receipt has Status 0; reason is then the revert reason, otherwise "".
func (x *Executor) applyTx(h *types.Header, blockHash common.Hash, getHash vm.GetHashFunc, statedb *state.StateDB,
	gp *core.GasPool, index int, tx *types.Transaction) (receipt *types.Receipt, reason string, err error) {
	if _, err := x.CheckTx(tx); err != nil {
		return nil, "", err
	}
	msg, err := core.TransactionToMessage(tx, x.signer, h.BaseFee)
	if err != nil {
		return nil, "", err
	}
	statedb.SetTxContext(tx.Hash(), index, 0)
	evm := vm.NewEVM(x.blockContext(h, getHash), statedb, x.eth, vm.Config{})
	snap, gsnap := statedb.Snapshot(), gp.Snapshot()
	result, err := core.ApplyMessage(evm, msg, gp)
	if err != nil {
		statedb.RevertToSnapshot(snap)
		gp.Set(gsnap)
		return nil, "", err
	}
	statedb.Finalise(x.rules(h))
	receipt = core.MakeReceipt(evm, result, statedb, h.Number, blockHash, h.Time, tx, gp.CumulativeUsed(), nil)
	if result.Failed() {
		reason = revertReason(result)
	}
	return receipt, reason, nil
}

// payReward credits the block reward (ChainConfig.BlockReward) to h.Coinbase. The fees of
// the transactions need no extra step: applyTx already paid them to the coinbase.
func (x *Executor) payReward(h *types.Header, statedb *state.StateDB) {
	if x.cfg.BlockReward != nil && x.cfg.BlockReward.Sign() > 0 {
		reward, _ := uint256.FromBig(x.cfg.BlockReward.Int())
		statedb.AddBalance(h.Coinbase, reward, tracing.BalanceIncreaseRewardMineBlock)
	}
}

// stateRoot is the root hash of statedb as it is now, for header.Root. Nothing is written
// to the state database.
func (x *Executor) stateRoot(h *types.Header, statedb *state.StateDB) common.Hash {
	return statedb.IntermediateRoot(x.rules(h))
}

// commitState writes statedb, the state after block h, to the state database, so that
// openState(h.Root) works from now on (for h's children and for the RPC server). Call it
// last, and only for a block you have found valid: statedb cannot be used after it.
func (x *Executor) commitState(h *types.Header, statedb *state.StateDB) error {
	if _, err := statedb.Commit(x.rules(h), h.Number.Uint64()); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	return nil
}

// txsRoot, receiptsRoot and logsBloom compute header.TxHash, header.ReceiptHash and
// header.Bloom from a block's transactions and receipts.
func txsRoot(txs []*types.Transaction) common.Hash {
	return types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
}

func receiptsRoot(receipts []*types.Receipt) common.Hash {
	return types.DeriveSha(types.Receipts(receipts), trie.NewStackTrie(nil))
}

func logsBloom(receipts []*types.Receipt) types.Bloom { return types.MergeBloom(receipts) }

// assemble makes the block with header h (a copy of it) and the given transactions, with
// TxHash, ReceiptHash, Bloom and UncleHash computed from them. Set the other fields that
// depend on execution (Root, GasUsed) in h first.
func assemble(h *types.Header, txs []*types.Transaction, receipts []*types.Receipt) *types.Block {
	h = types.CopyHeader(h)
	h.Bloom = logsBloom(receipts)
	return types.NewBlock(h, &types.Body{Transactions: txs}, receipts, trie.NewStackTrie(nil))
}

// CallMsg is a read-only call (eth_call, eth_estimateGas).
type CallMsg struct {
	From  common.Address
	To    *common.Address // nil: contract creation
	Gas   uint64          // 0: the block gas limit
	Value *big.Int
	Data  []byte
}

// Call runs msg on a copy of statedb, in the context of block h, and returns the result.
// Nonces and balances for gas are not checked; value transfers still need the balance.
func (x *Executor) Call(statedb *state.StateDB, h *types.Header, getHash vm.GetHashFunc, m CallMsg) (*core.ExecutionResult, error) {
	gas := m.Gas
	if gas == 0 || gas > x.cfg.GasLimit {
		gas = x.cfg.GasLimit
	}
	value := new(uint256.Int)
	if m.Value != nil {
		var overflow bool
		if value, overflow = uint256.FromBig(m.Value); overflow {
			return nil, errors.New("value overflows uint256")
		}
	}
	msg := &core.Message{
		From: m.From, To: m.To, Value: value, GasLimit: gas, Data: m.Data,
		GasPrice: new(uint256.Int), GasFeeCap: new(uint256.Int), GasTipCap: new(uint256.Int),
		SkipNonceChecks: true, SkipTransactionChecks: true,
	}
	sdb := statedb.Copy()
	evm := vm.NewEVM(x.blockContext(h, getHash), sdb, x.eth, vm.Config{NoBaseFee: true})
	return core.ApplyMessage(evm, msg, core.NewGasPool(gas))
}

// EstimateGas finds the smallest gas limit with which msg succeeds, by binary search,
// and returns it with a 25% margin (capped at the block gas limit), so that a small
// change of state between the estimate and the inclusion does not run out of gas.
func (x *Executor) EstimateGas(statedb *state.StateDB, h *types.Header, getHash vm.GetHashFunc, m CallMsg) (uint64, error) {
	m.Gas = x.cfg.GasLimit
	res, err := x.Call(statedb, h, getHash, m)
	if err != nil {
		return 0, err
	}
	if res.Failed() {
		return 0, &RevertError{Reason: revertReason(res), Data: res.Revert()}
	}
	lo, hi := res.UsedGas-1, x.cfg.GasLimit
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		m.Gas = mid
		r, err := x.Call(statedb, h, getHash, m)
		if err != nil || r.Failed() {
			lo = mid
		} else {
			hi = mid
		}
	}
	return min(hi+hi/4, x.cfg.GasLimit), nil
}

// RevertError is a call that reverted, with its reason when the contract gave one.
type RevertError struct {
	Reason string
	Data   []byte
}

func (e *RevertError) Error() string {
	if e.Reason == "" {
		return "execution reverted"
	}
	return "execution reverted: " + e.Reason
}

// revertReason decodes require/revert messages; otherwise it describes the failure.
func revertReason(r *core.ExecutionResult) string {
	if reason, err := abi.UnpackRevert(r.Revert()); err == nil && reason != "" {
		return reason
	}
	if r.Err != nil {
		return r.Err.Error()
	}
	return "reverted"
}
