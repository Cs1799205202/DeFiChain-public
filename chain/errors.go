package chain

// Framework file: the errors that tests check with errors.Is. When you reject a block or
// header, wrap one of these, e.g. fmt.Errorf("%w: got %d want %d", ErrInvalidDifficulty, a, b).

import (
	"errors"
	"log"
	"sync"
)

var (
	// ErrUnknownParent: the block's parent is not stored here (yet). Not a reason to
	// distrust the block: fetch the parent and try again.
	ErrUnknownParent = errors.New("unknown parent")
	// ErrFutureBlock: the timestamp is more than MaxFutureTime ahead of the local clock.
	// The block may become valid later; it is not stored now.
	ErrFutureBlock = errors.New("block in the future")

	// Header rules (VerifyHeader).
	ErrInvalidNumber     = errors.New("invalid block number")
	ErrInvalidTimestamp  = errors.New("invalid timestamp")
	ErrInvalidDifficulty = errors.New("invalid difficulty")
	ErrInvalidGasLimit   = errors.New("invalid gas limit")
	ErrInvalidGasUsed    = errors.New("gas used exceeds gas limit")
	ErrExtraTooLong      = errors.New("extra data too long")
	ErrInvalidUncles     = errors.New("uncles are not allowed")
	ErrInvalidBaseFee    = errors.New("invalid base fee")
	ErrInvalidMixDigest  = errors.New("mix digest does not match the PoW hash")
	ErrInvalidPoW        = errors.New("PoW hash does not meet the difficulty target")

	// ErrInvalidBlock: the body does not match the header, or executing the block does
	// not give the state root, receipts, bloom or gas used the header commits to.
	ErrInvalidBlock = errors.New("invalid block")

	// The txpool (Add). Nonce and balance use go-ethereum's core.ErrNonceTooLow and
	// core.ErrInsufficientFunds.
	ErrReplaceUnderpriced = errors.New("replacement transaction underpriced (needs a strictly higher tip)")
	ErrPoolFull           = errors.New("txpool is full")

	// ErrNotImplemented is returned by the skeleton's unfinished functions.
	ErrNotImplemented = errors.New("not implemented")
)

// MaxPoolTxs is the capacity of the txpool, pending and queued together.
const MaxPoolTxs = 8192

var todoOnce sync.Map

// todo logs, once per function, that a skeleton function is still unimplemented.
func todo(l *log.Logger, name string) {
	if _, seen := todoOnce.LoadOrStore(name, true); !seen && l != nil {
		l.Printf("TODO: %s is not implemented yet", name)
	}
}
