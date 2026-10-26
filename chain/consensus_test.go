package chain

// Tests for task 2 (consensus.go). Run: go test ./chain -run TestConsensus -v

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestConsensusCalcDifficulty(t *testing.T) {
	cfg := labGenesis().Config // target 2 s, divisor 16, minimum 1024
	cases := []struct {
		parentDiff int64
		dt         uint64
		want       int64
		why        string
	}{
		{160000, 1, 170000, "1 s < target: x = 1, +160000/16"},
		{160000, 2, 160000, "2 s: x = 1 - 2/2 = 0, unchanged"},
		{160000, 3, 160000, "3 s: x = 1 - 3/2 = 1 - 1 = 0 (integer division)"},
		{160000, 4, 150000, "4 s: x = 1 - 2 = -1"},
		{160000, 10, 120000, "10 s: x = 1 - 5 = -4"},
		{2000, 10, 1500, "2000 + 2000/16 * -4 = 2000 - 500"},
		{1025, 1, 1089, "1025/16 = 64 (integer division)"},
		{160000, 400, 1024, "very slow: x = -99, far below the minimum: clamped to 1024"},
		{1024, 100, 1024, "never below the minimum"},
	}
	for _, c := range cases {
		parent := &types.Header{Number: big.NewInt(7), Time: 1000, Difficulty: big.NewInt(c.parentDiff)}
		got := CalcDifficulty(cfg, 1000+c.dt, parent)
		if got == nil || got.Cmp(big.NewInt(c.want)) != 0 {
			t.Errorf("CalcDifficulty(parent %d, +%ds) = %v, want %d (%s)", c.parentDiff, c.dt, got, c.want, c.why)
		}
		if parent.Difficulty.Int64() != c.parentDiff {
			t.Fatal("CalcDifficulty modified parent.Difficulty: compute with new big.Ints")
		}
	}
}

// labSealedChild returns a valid, sealed header for an empty child of genesis, dt seconds
// after it, and genesis's header. No execution is needed: VerifyHeader does not look at
// the state root (processBlock does).
func labSealedChild(t *testing.T, dt uint64) (*types.Header, *types.Header) {
	t.Helper()
	cfg := labGenesis().Config
	db := newStateDatabase()
	gb, err := GenesisBlock(labGenesis(), db)
	if err != nil {
		t.Fatal(err)
	}
	g := gb.Header()
	d := CalcDifficulty(cfg, g.Time+dt, g)
	if d == nil || d.Sign() <= 0 {
		t.Fatalf("CalcDifficulty returned %v: not implemented yet?", d)
	}
	h := &types.Header{ParentHash: g.Hash(), Number: big.NewInt(1), Time: g.Time + dt, Coinbase: labMinerA, Difficulty: d,
		GasLimit: cfg.GasLimit, BaseFee: new(big.Int), Root: g.Root, UncleHash: types.EmptyUncleHash,
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
	return labReseal(t, h), g
}

// labReseal seals h again (after a test changed it) with your Seal.
func labReseal(t *testing.T, h *types.Header) *types.Header {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := Seal(ctx, types.NewBlockWithHeader(h), nil)
	if err != nil || b == nil {
		t.Fatalf("Seal: %v", err)
	}
	return b.Header()
}

func TestConsensusVerifyHeader(t *testing.T) {
	cfg := labGenesis().Config
	h, parent := labSealedChild(t, 3)
	now := uint64(labNow.Unix())
	if err := VerifyHeader(cfg, h, parent, now); err != nil {
		t.Fatalf("VerifyHeader(valid header) = %v, want nil", err)
	}
	check := func(name string, h *types.Header, now uint64, want error) {
		t.Helper()
		err := VerifyHeader(cfg, h, parent, now)
		if !errors.Is(err, want) {
			t.Errorf("%s: VerifyHeader = %v, want an error wrapping %v", name, err, want)
		}
	}
	bad := types.CopyHeader(h)
	bad.Number = big.NewInt(2)
	check("number = parent+2", labReseal(t, bad), now, ErrInvalidNumber)

	bad = types.CopyHeader(h)
	bad.Time = parent.Time
	bad.Difficulty = CalcDifficulty(cfg, parent.Time, parent)
	check("time = parent time", labReseal(t, bad), now, ErrInvalidTimestamp)

	check("timestamp too far ahead of the clock", h, h.Time-cfg.MaxFutureTime-1, ErrFutureBlock)

	bad = types.CopyHeader(h)
	bad.Difficulty = new(big.Int).Add(h.Difficulty, big.NewInt(1))
	check("difficulty off by one", labReseal(t, bad), now, ErrInvalidDifficulty)

	bad = types.CopyHeader(h)
	bad.Extra = []byte("changed after sealing")
	check("header changed after sealing", bad, now, ErrInvalidMixDigest)
}
