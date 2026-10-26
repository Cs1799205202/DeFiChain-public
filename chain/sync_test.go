package chain

// Tests for task 7 (sync.go). Run: go test ./chain -run TestSync -v
// The peers are fake (labFakeNet): no network, no timing.

import (
	"testing"
	"time"
)

func TestSyncHandleBlock(t *testing.T) {
	bc, lb := labChain(t), labNewBuilder(t)
	net := labNewFakeNet()
	blocks := lb.chain(lb.genesis(), 3, 2, labMinerA)
	net.add("p1", lb, blocks[2])
	h := NewHandler(bc, net, bc.log)

	// A block whose parent we have: insert it and relay it to everyone but the sender.
	labDo(t, 10*time.Second, "HandleBlock", func() { h.HandleBlock(blocks[0], "p1") })
	labWantCanonical(t, bc, blocks[0])
	sent := net.broadcasts()
	if len(sent) != 1 || sent[0].block.Hash() != blocks[0].Hash() || sent[0].except != "p1" {
		t.Fatalf("after a new valid block: broadcasts %+v, want one relay of it, except to its sender p1", sent)
	}
	// The same block again: nothing happens, in particular no relay (or blocks would
	// bounce between nodes forever).
	labDo(t, 10*time.Second, "HandleBlock", func() { h.HandleBlock(blocks[0], "p1") })
	if sent := net.broadcasts(); len(sent) != 0 {
		t.Fatalf("a block we already had was relayed again: %+v", sent)
	}
	// A block whose parent we miss: fetch the ancestors from the sender.
	labDo(t, 10*time.Second, "HandleBlock", func() { h.HandleBlock(blocks[2], "p1") })
	labWantCanonical(t, bc, blocks[2])
	if net.requests["p1"] == 0 {
		t.Fatal("the missing parent was not requested from the sender (RequestAncestors)")
	}
}

func TestSyncOnceFollowsHeaviestPeer(t *testing.T) {
	bc := labChain(t)
	heavy, light := labNewBuilder(t), labNewBuilder(t)
	hb := heavy.chain(heavy.genesis(), 6, 2, labMinerA)
	lbk := light.chain(light.genesis(), 2, 2, labMinerB)
	net := labNewFakeNet()
	net.add("light", light, lbk[1])
	net.add("heavy", heavy, hb[5])
	h := NewHandler(bc, net, bc.log)
	labDo(t, 20*time.Second, "SyncOnce", h.SyncOnce)
	labWantCanonical(t, bc, hb[5])
	labDo(t, 20*time.Second, "SyncOnce", h.SyncOnce) // nothing more to do
	labWantCanonical(t, bc, hb[5])
}
