package chain

// ★ Student file (Part 1, task 7): block propagation and synchronisation.
//
// Two mechanisms keep nodes in agreement:
//
//   - Push: a new block is broadcast to all peers, and every node that accepts it relays it
//     to its own peers, so it crosses the network even if not everyone is connected to
//     everyone. If a pushed block's parent is unknown (we missed some blocks), fetch the
//     missing ancestors from the peer that sent it.
//   - Pull: pushes get lost (a peer was down, partitioned, a message was dropped). So every
//     node also asks its peers for their head every SyncInterval, and fetches the chain of
//     any peer whose head has a higher total difficulty than its own. That is what makes
//     nodes converge after a partition heals even if nobody mines a new block.
//
// Tests: go test ./chain -run TestSync -v, then go test ./tests -v (several real nodes).

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Packages you will probably need (delete this line once you use it).
var _ = errors.Is

// SyncInterval is how often the pull loop runs. Tests rely on nodes converging within a
// few intervals after a partition heals, so keep it at most 2 s.
const SyncInterval = time.Second

// maxSyncDepth bounds how far back fetchChain walks, so a lying peer cannot make us
// download forever.
const maxSyncDepth = 100_000

// Handler connects the block chain to the network.
type Handler struct {
	bc   *BlockChain
	net  PeerNet
	log  *log.Logger
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewHandler creates the sync handler.
func NewHandler(bc *BlockChain, net PeerNet, logger *log.Logger) *Handler {
	return &Handler{bc: bc, net: net, log: logger, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start runs the pull loop (SyncOnce every SyncInterval) in the background until Stop.
func (h *Handler) Start() {
	go func() {
		defer close(h.done)
		t := time.NewTicker(SyncInterval)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				h.SyncOnce()
			}
		}
	}()
}

// Stop ends the pull loop. Harmless if Start was never called.
func (h *Handler) Stop() { h.once.Do(func() { close(h.stop) }) }

// HandleBlock is called for every block a peer pushes to us, one at a time, in arrival
// order. from is the sending peer's id.
//
//   - Already known: ignore it (this is what ends the relay).
//   - Parent unknown: fetch the missing ancestors from `from` (fetchChain).
//   - Valid and new (canonical or not): relay it to every peer except `from`.
//   - Invalid: drop it (log it), do not relay.
//
// TODO. errors.Is(err, ErrUnknownParent) tells the parent-unknown case apart.
func (h *Handler) HandleBlock(b *types.Block, from string) {
	todo(h.log, "Handler.HandleBlock")
}

// fetchChain downloads the block `tip` and every ancestor of it that we do not have from
// peer (RequestAncestors, newest first, as many requests as needed), inserts them oldest
// first, and relays tip if it was inserted.
//
// TODO. Ask for MaxAncestors blocks at a time, starting from tip, then from the parent of
// the oldest block received, until you reach a block whose parent you have. Check what
// the peer sends: the first block must be the one you asked for, and each next one the
// parent of the previous. Stop after maxSyncDepth blocks. Then h.bc.InsertChain(oldest
// first). Remember: genesis is never sent, and you always have it.
func (h *Handler) fetchChain(peer string, tip common.Hash) error {
	return ErrNotImplemented
}

// SyncOnce asks every peer for its head and, if the best one has a higher total
// difficulty than ours, fetches its chain. Peers that do not answer are skipped.
//
// TODO. Ask all peers (in parallel is better: a slow peer should not delay the others).
// Pick the heaviest head we do not have; if fetching from that peer fails, try the next
// best one. A peer's claimed TD is only a hint: InsertBlock computes the real one.
func (h *Handler) SyncOnce() {
	todo(h.log, "Handler.SyncOnce")
}
