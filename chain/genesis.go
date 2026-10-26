package chain

// Framework file: builds block 0 from a genesis spec.

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"

	"defichain/spec"
)

// newStateDatabase is an in-memory state database that keeps the state of every block
// it ever committed (go-ethereum's hash-based trie database never prunes on its own).
// That is what makes a reorg cheap: switching to another branch needs no re-execution,
// because the state after each of its blocks is still there. Nodes do not persist
// anything to disk: a restarted node starts from genesis and syncs from its peers.
func newStateDatabase() state.Database {
	db := rawdb.NewMemoryDatabase()
	return state.NewDatabase(triedb.NewDatabase(db, nil), state.NewCodeDB(db))
}

// GenesisBlock builds block 0 and writes its state into db.
func GenesisBlock(g *spec.Genesis, db state.Database) (*types.Block, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	statedb, err := state.New(types.EmptyRootHash, db)
	if err != nil {
		return nil, err
	}
	for addr, acc := range g.Alloc {
		bal, overflow := uint256.FromBig(acc.Balance.Int())
		if overflow {
			return nil, fmt.Errorf("alloc %s: balance overflows uint256", addr)
		}
		statedb.AddBalance(common.HexToAddress(addr), bal, tracing.BalanceIncreaseGenesisBalance)
	}
	eth := ethConfig(g.Config.ChainID)
	rules := eth.Rules(new(big.Int), true, g.Timestamp)
	root, err := statedb.Commit(rules, 0)
	if err != nil {
		return nil, err
	}
	h := &types.Header{
		ParentHash:  common.Hash{},
		UncleHash:   types.EmptyUncleHash,
		Coinbase:    common.Address{},
		Root:        root,
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Difficulty:  new(big.Int).SetUint64(g.Difficulty),
		Number:      new(big.Int),
		GasLimit:    g.Config.GasLimit,
		Time:        g.Timestamp,
		Extra:       []byte(g.ExtraData),
		BaseFee:     new(big.Int),
	}
	return types.NewBlockWithHeader(h), nil
}
