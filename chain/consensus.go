package chain

// ★ Student file (Part 1, task 2): difficulty adjustment and header validation.
// Tests: go test ./chain -run TestConsensus -v

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"

	"defichain/spec"
)

var _ = fmt.Errorf // you will need it (delete this line then)

// CalcDifficulty is the difficulty a child of parent with timestamp `time` must have.
// It is Ethereum's Homestead rule (EIP-2) with our parameters:
//
//	x    = max(1 - (time - parent.Time) / TargetBlockTime, -99)    (integer division)
//	diff = parent.Difficulty + parent.Difficulty / DifficultyBoundDivisor * x
//	diff = max(diff, MinimumDifficulty)
//
// A block found quickly (in less than TargetBlockTime) raises the difficulty by
// parent/Div; a slow one lowers it, by more the slower it was. Only the timestamps say
// how fast blocks were found, which is why miners must not lie about time too much (see
// MaxFutureTime). VerifyHeader rejects time <= parent.Time before calling this; treat
// that case as time - parent.Time = 0.
//
// TODO: careful with unsigned arithmetic: time - parent.Time underflows if time is smaller,
// and converting a huge uint64 quotient to int64 can overflow; clamp before converting.
// Compute with new big.Ints: never modify parent.Difficulty.
func CalcDifficulty(cfg spec.ChainConfig, time uint64, parent *types.Header) *big.Int {
	return nil
}

// VerifyHeader checks every rule a header must follow, given its parent and the local
// clock `now` (Unix seconds). The caller has already found parent by h.ParentHash.
// Returns nil, or an error wrapping the sentinel of the first rule broken:
//
//	1. h.Number == parent.Number + 1                           ErrInvalidNumber
//	2. h.Time > parent.Time                                    ErrInvalidTimestamp
//	3. h.Time <= now + MaxFutureTime                           ErrFutureBlock
//	4. h.Difficulty == CalcDifficulty(cfg, h.Time, parent)      ErrInvalidDifficulty
//	5. h.GasLimit == cfg.GasLimit                              ErrInvalidGasLimit
//	6. h.GasUsed <= h.GasLimit                                 ErrInvalidGasUsed
//	7. len(h.Extra) <= 32                                      ErrExtraTooLong
//	8. h.UncleHash == types.EmptyUncleHash                     ErrInvalidUncles
//	9. h.BaseFee != nil && h.BaseFee == 0                      ErrInvalidBaseFee
//	10. VerifySeal(h) == nil                                   (VerifySeal's error)
//
// The cheap checks come first, the seal last: a node should not spend effort on a block
// that is wrong anyway. (Hashing the seal is cheap here, but it is not with Ethash.)
//
// TODO: one `if` per rule, in this order. Wrap the sentinel so errors.Is finds it and
// the message says what is wrong: fmt.Errorf("%w: %d, want %d", ErrInvalidGasLimit, a, b).
// Any field may be nil in a malicious header (h.Number, h.Difficulty, h.BaseFee): an
// error, never a panic.
func VerifyHeader(cfg spec.ChainConfig, h, parent *types.Header, now uint64) error {
	return ErrNotImplemented
}
