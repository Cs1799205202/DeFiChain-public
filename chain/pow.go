package chain

// ★ Student file (Part 1, task 1): the proof-of-work seal.
//
// A block is sealed by finding a nonce such that a hash of the header with that nonce is
// small enough. Our PoW function is a simplified Hashcash, not Ethereum's Ethash (Ethash
// adds a large memory-hard dataset so that mining needs RAM, not just CPU; the consensus
// logic is the same):
//
//	sealHash = Keccak256(RLP(header with Nonce and MixDigest set to zero))
//	powHash  = Keccak256(sealHash || nonce)          (nonce: 8 bytes, big-endian)
//	valid   <=> header.MixDigest == powHash  and  powHash <= 2^256 / difficulty
//
// MixDigest stores the PoW hash in the header, so a verifier can reject a block with a
// wrong MixDigest by comparing 32 bytes before doing any other work.
//
// The exact byte layout, and test vectors, are in docs/SPEC.md. Tests: go test ./chain -run TestPow -v

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"math/rand"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Packages you will probably need (delete these lines once you use them).
var (
	_ = binary.BigEndian
	_ = fmt.Errorf
	_ = rand.Uint64
	_ = crypto.Keccak256Hash
	_ = rlp.EncodeToBytes
	_ = context.Background
)

// two256 is 2^256.
var two256 = new(big.Int).Lsh(common.Big1, 256)

// SealHash is the hash a miner works on: the header without its seal (Nonce, MixDigest).
// Every other field, including the difficulty and the timestamp, is covered, so changing
// any of them invalidates the seal.
//
// TODO: copy the header (types.CopyHeader: never modify the caller's header), zero its
// Nonce and MixDigest, RLP-encode it (rlp.EncodeToBytes) and return the Keccak256 hash.
func SealHash(h *types.Header) common.Hash {
	return common.Hash{}
}

// PowHash is Keccak256(sealHash || nonce), the nonce as 8 big-endian bytes.
//
// TODO: 40 bytes, the 32 of sealHash then the nonce (binary.BigEndian.PutUint64).
func PowHash(sealHash common.Hash, nonce uint64) common.Hash {
	return common.Hash{}
}

// Target is the largest PoW hash a block of this difficulty may have: 2^256 / difficulty
// (integer division). Doubling the difficulty halves the target, so it takes twice as many
// attempts on average to find a nonce.
//
// TODO: one line, with two256. Return a new big.Int; do not modify the argument.
func Target(difficulty *big.Int) *big.Int {
	return nil
}

// VerifySeal checks h's proof of work: MixDigest must equal PowHash(SealHash(h), nonce),
// and that hash, read as a big-endian number, must be <= Target(h.Difficulty). Errors
// wrap ErrInvalidDifficulty (difficulty missing or <= 0), ErrInvalidMixDigest or
// ErrInvalidPoW.
//
// TODO: check the difficulty first (nil or <= 0 must be an error, not a panic). The nonce
// is h.Nonce.Uint64(). new(big.Int).SetBytes(hash[:]) reads a hash as a number.
func VerifySeal(h *types.Header) error {
	return ErrNotImplemented
}

// sealBatch is how many nonces Seal tries between calls to throttle.Wait.
const sealBatch = 1024

// Seal searches for a nonce that makes b's header valid and returns the sealed block
// (b.WithSeal(header with Nonce and MixDigest set)). It tries nonces in batches of
// sealBatch and calls throttle.Wait(ctx, sealBatch) after each batch, which both caps the
// hashrate (a nil throttle means no cap) and returns an error once ctx is cancelled: then
// Seal returns that error. Start from a random nonce so that two miners working on the
// same header do not try the same nonces.
//
// TODO:
//   - h := b.Header() is already a copy; check h.Difficulty like VerifySeal does;
//   - compute SealHash(h) and the target once, outside the loop;
//   - nonce := rand.Uint64(); then loop: try sealBatch nonces (nonce, nonce+1, ...); when
//     PowHash(seal, nonce) <= target, set h.Nonce = types.EncodeNonce(nonce) and
//     h.MixDigest = that hash, and return b.WithSeal(h), nil;
//   - after each batch: if err := throttle.Wait(ctx, sealBatch); err != nil { return nil, err }.
func Seal(ctx context.Context, b *types.Block, throttle *Throttle) (*types.Block, error) {
	return nil, ErrNotImplemented
}
