package chain

// Tests for task 6 (miner.go). Run: go test ./chain -run TestMiner -v

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestMinerMineBlock(t *testing.T) {
	bc := labChain(t)
	lb := labNewBuilder(t)
	tx := lb.tx(labAliceKey, 0, labBob, 777)
	if ok, err := bc.pool.Add(tx); !ok || err != nil {
		t.Fatalf("pool.Add: %v %v", ok, err)
	}
	rec := &labRecorder{}
	m := NewMiner(bc, bc.pool, rec, labMinerA, nil, bc.log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 1; i <= 3; i++ {
		var b *types.Block
		var err error
		labDo(t, 35*time.Second, "MineBlock", func() { b, err = m.MineBlock(ctx) })
		if err != nil || b == nil {
			t.Fatalf("MineBlock #%d: block %v, error %v", i, b, err)
		}
		head := bc.CurrentBlock()
		if head.NumberU64() != uint64(i) {
			t.Fatalf("after mining block %d the head is block %d: insert the mined block", i, head.NumberU64())
		}
		if head.Coinbase() != labMinerA {
			t.Fatalf("block %d coinbase %s, want the miner's address", i, head.Coinbase().Hex())
		}
		if err := VerifyHeader(bc.Config(), head.Header(), bc.GetBlock(head.ParentHash()).Header(), uint64(labNow.Unix())); err != nil {
			t.Fatalf("the mined block does not pass VerifyHeader: %v", err)
		}
	}
	if info := bc.LookupTx(tx.Hash()); info == nil || info.Block.NumberU64() != 1 {
		t.Fatal("the pending transfer is not in block 1: build the block from pool.Pending()")
	}
	if rec.count() != 3 {
		t.Fatalf("the miner broadcast %d blocks, want 3 (one per mined block)", rec.count())
	}
	st, _, _ := bc.HeadState()
	if bal := st.GetBalance(labMinerA).ToBig(); bal.Cmp(new(big.Int).Mul(big.NewInt(3), bc.cfg.BlockReward.Int())) < 0 {
		t.Fatalf("coinbase balance %s, want at least 3 block rewards", bal)
	}
}
