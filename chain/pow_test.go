package chain

// Tests for task 1 (pow.go). Run: go test ./chain -run TestPow -v

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// labVectorHeader is the header of the test vectors in docs/SPEC.md.
func labVectorHeader() *types.Header {
	return &types.Header{
		ParentHash:  common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
		UncleHash:   types.EmptyUncleHash,
		Coinbase:    common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Root:        common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333"),
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Difficulty:  big.NewInt(131072),
		Number:      big.NewInt(42),
		GasLimit:    30_000_000,
		GasUsed:     21000,
		Time:        1_700_000_042,
		Extra:       []byte("lab2"),
		BaseFee:     new(big.Int),
		MixDigest:   common.HexToHash("0x4444444444444444444444444444444444444444444444444444444444444444"),
		Nonce:       types.EncodeNonce(0x0123456789abcdef),
	}
}

func TestPowSealHashVector(t *testing.T) {
	want := common.HexToHash("0x9b0a120487fb0dde8f9f81e4fe98960da85f7c1d704ccf9e245b36defa0857ad")
	if got := SealHash(labVectorHeader()); got != want {
		t.Fatalf("SealHash = %s, want %s (see docs/SPEC.md: RLP of the header with Nonce and MixDigest zeroed)", got.Hex(), want.Hex())
	}
}

func TestPowSealHashIgnoresSealCoversRest(t *testing.T) {
	h := labVectorHeader()
	base := SealHash(h)
	h2 := types.CopyHeader(h)
	h2.Nonce, h2.MixDigest = types.EncodeNonce(7), common.Hash{9}
	if SealHash(h2) != base {
		t.Fatal("SealHash changed with Nonce/MixDigest: it must not cover the seal itself")
	}
	for name, change := range map[string]func(*types.Header){
		"Time":       func(h *types.Header) { h.Time++ },
		"Difficulty": func(h *types.Header) { h.Difficulty = big.NewInt(131073) },
		"Coinbase":   func(h *types.Header) { h.Coinbase[0] ^= 1 },
		"Root":       func(h *types.Header) { h.Root[0] ^= 1 },
		"Extra":      func(h *types.Header) { h.Extra = []byte("lab3") },
	} {
		h3 := types.CopyHeader(h)
		change(h3)
		if SealHash(h3) == base {
			t.Errorf("SealHash did not change when %s changed: the seal must cover it", name)
		}
	}
	if h.Nonce != types.EncodeNonce(0x0123456789abcdef) || h.MixDigest[0] != 0x44 {
		t.Error("SealHash modified its argument: work on a copy (types.CopyHeader)")
	}
}

func TestPowHashVector(t *testing.T) {
	seal := common.HexToHash("0x9b0a120487fb0dde8f9f81e4fe98960da85f7c1d704ccf9e245b36defa0857ad")
	for nonce, want := range map[uint64]string{
		0:                  "0xa770a6da07c0fa719824008e1d0dc6dc5cf15cba6df0d2a9e124edec49cc6ea1",
		1:                  "0x230a3251fd52f2b08d5c4a4b36c03425d5928585acc53cac91764b62d4194fa9",
		0x0123456789abcdef: "0xa6a5ef65797aa8dc47bc42543c78e596d074f8e69d4f34899c9d62874409b988",
	} {
		if got := PowHash(seal, nonce); got != common.HexToHash(want) {
			t.Errorf("PowHash(seal, %#x) = %s, want %s (Keccak256 of sealHash followed by the nonce as 8 big-endian bytes)",
				nonce, got.Hex(), want)
		}
	}
}

func TestPowTarget(t *testing.T) {
	two256 := new(big.Int).Lsh(big.NewInt(1), 256)
	for _, d := range []int64{1, 2, 3, 1000, 131072} {
		got := Target(big.NewInt(d))
		if got == nil {
			t.Fatalf("Target(%d) = nil", d)
		}
		if want := new(big.Int).Div(two256, big.NewInt(d)); got.Cmp(want) != 0 {
			t.Errorf("Target(%d) = %s, want 2^256/%d = %s", d, got, d, want)
		}
	}
}

// labSealTestBlock is a block of the vector header with a small difficulty.
func labSealTestBlock(difficulty int64) *types.Block {
	h := labVectorHeader()
	h.Difficulty = big.NewInt(difficulty)
	h.Nonce, h.MixDigest = types.BlockNonce{}, common.Hash{}
	return types.NewBlockWithHeader(h)
}

func TestPowSealAndVerify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := labSealTestBlock(4096)
	var sealed *types.Block
	var err error
	labDo(t, 35*time.Second, "Seal", func() { sealed, err = Seal(ctx, b, nil) })
	if err != nil || sealed == nil {
		t.Fatalf("Seal: block %v, error %v", sealed, err)
	}
	h := sealed.Header()
	if SealHash(h) != SealHash(b.Header()) {
		t.Fatal("the sealed block's header differs from the input in more than Nonce and MixDigest")
	}
	if err := VerifySeal(h); err != nil {
		t.Fatalf("VerifySeal(sealed block) = %v, want nil", err)
	}
	if pow := PowHash(SealHash(h), h.Nonce.Uint64()); h.MixDigest != pow {
		t.Fatalf("MixDigest %s, want the PoW hash %s", h.MixDigest.Hex(), pow.Hex())
	}
	if tgt := Target(h.Difficulty); tgt == nil {
		t.Fatal("Target returned nil")
	} else if pow := PowHash(SealHash(h), h.Nonce.Uint64()); new(big.Int).SetBytes(pow[:]).Cmp(tgt) > 0 {
		t.Fatal("the sealed block's PoW hash is above the target")
	}
	// A different nonce with the old MixDigest: the mix digest no longer matches.
	bad := types.CopyHeader(h)
	bad.Nonce = types.EncodeNonce(h.Nonce.Uint64() + 1)
	if err := VerifySeal(bad); !errors.Is(err, ErrInvalidMixDigest) {
		t.Fatalf("VerifySeal(wrong nonce) = %v, want ErrInvalidMixDigest", err)
	}
}

func TestPowVerifySealRejectsWeakHash(t *testing.T) {
	// Find a nonce whose PoW hash is above the target, and set MixDigest to that hash: the
	// mix digest is consistent, but the work is not enough.
	h := labSealTestBlock(1 << 20).Header()
	seal, target := SealHash(h), Target(h.Difficulty)
	if target == nil {
		t.Fatal("Target returned nil (implement Target first)")
	}
	found := false
	for n := uint64(0); n < 1000 && !found; n++ {
		pow := PowHash(seal, n)
		if new(big.Int).SetBytes(pow[:]).Cmp(target) > 0 {
			h.Nonce, h.MixDigest, found = types.EncodeNonce(n), pow, true
		}
	}
	if !found {
		t.Fatal("no PoW hash above the target in 1000 nonces: is PowHash implemented?")
	}
	if err := VerifySeal(h); !errors.Is(err, ErrInvalidPoW) {
		t.Fatalf("VerifySeal(hash above target) = %v, want ErrInvalidPoW", err)
	}
}

func TestPowSealStopsWhenCancelled(t *testing.T) {
	easy, err := Seal(context.Background(), labSealTestBlock(64), nil)
	if err != nil || easy == nil {
		t.Fatalf("Seal of an easy block: %v (implement Seal first)", err)
	}
	b := labSealTestBlock(0).Header()
	b.Difficulty = new(big.Int).Lsh(big.NewInt(1), 62) // never found in a test's lifetime
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	labDo(t, 5*time.Second, "Seal with a cancelled context", func() { _, err = Seal(ctx, types.NewBlockWithHeader(b), nil) })
	if err == nil {
		t.Fatal("Seal returned no error after its context was cancelled")
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("Seal gave up after %s, before its context was cancelled", time.Since(start))
	}
}

func TestPowSealUsesThrottle(t *testing.T) {
	b := labSealTestBlock(0).Header()
	b.Difficulty = new(big.Int).Lsh(big.NewInt(1), 62)
	th := NewThrottle(50_000) // 50 kH/s: about 25,000 hashes in half a second
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	labDo(t, 5*time.Second, "throttled Seal", func() { Seal(ctx, types.NewBlockWithHeader(b), th) }) //nolint:errcheck
	th.mu.Lock()
	counted := th.count
	th.mu.Unlock()
	if counted == 0 {
		t.Fatal("Seal never called throttle.Wait: call it after every batch of sealBatch nonces")
	}
	if counted > 100_000 {
		t.Fatalf("Seal reported %.0f hashes to a 50 kH/s throttle in 0.5 s: it is not waiting", counted)
	}
}
