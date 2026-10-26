package tests

// Multi-node tests (Part 1, all tasks together). Run: go test ./tests -v
// Each test starts its own nodes; the node logs are printed when a test fails.

import (
	"math/big"
	"testing"
	"time"

	"defichain/client"
	"defichain/devnet"
)

// Blocks mined on one node reach every node, whoever mines.
func TestClusterPropagation(t *testing.T) {
	d := startNet(t, devnet.Options{N: 3})
	blocks := mine(t, d.Nodes[0], 5)
	if head := converge(t, d, 20*time.Second); head.Hash != blocks[4].Hash {
		t.Fatalf("converged on %d %s, want n1's block 5", head.Number, head.Hash.TerminalString())
	}
	blocks = mine(t, d.Nodes[2], 3)
	if head := converge(t, d, 20*time.Second); head.Hash != blocks[2].Hash || head.Number != 8 {
		t.Fatalf("converged on %d %s, want n3's block 8", head.Number, head.Hash.TerminalString())
	}
}

// Two sides of a partition mine separately; after the heal everyone follows the heavier.
func TestClusterPartitionHeal(t *testing.T) {
	d := startNet(t, devnet.Options{N: 3})
	n1, n2, n3 := d.Nodes[0], d.Nodes[1], d.Nodes[2]
	mine(t, n1, 2)
	converge(t, d, 20*time.Second)
	if err := d.Partition([]*devnet.Node{n1}, []*devnet.Node{n2, n3}); err != nil {
		t.Fatal(err)
	}
	a := mine(t, n1, 2) // light side
	b := mine(t, n2, 4) // heavy side
	if err := d.WaitBlock(timeout(t, 20*time.Second), n3, b[3].Number, b[3].Hash); err != nil {
		t.Fatalf("partitioned: n3 should follow n2: %v", err)
	}
	if h, _ := n1.Head(); h.Hash != a[1].Hash {
		t.Fatalf("partitioned: n1's head is %d %s, want its own block %d (n1 cannot hear from n2 and n3)",
			h.Number, h.Hash.TerminalString(), a[1].Number)
	}
	if err := d.Heal(); err != nil {
		t.Fatal(err)
	}
	c := mine(t, n2, 1) // its parent is unknown to n1: n1 must fetch the heavier branch
	head := converge(t, d, 30*time.Second)
	if head.Hash != c[0].Hash || head.Number != 7 {
		t.Fatalf("converged on %d %s, want n2's block 7", head.Number, head.Hash.TerminalString())
	}
	if reorgs(t, n1) == 0 {
		t.Fatal("n1 switched branches but reports no reorg")
	}
}

// A crashed node loses everything (no disk); restarted, it syncs the chain from its peers.
func TestClusterRestart(t *testing.T) {
	d := startNet(t, devnet.Options{N: 3})
	n1, n3 := d.Nodes[0], d.Nodes[2]
	mine(t, n1, 3)
	converge(t, d, 20*time.Second)
	n3.Kill()
	mine(t, n1, 5)
	converge(t, d, 20*time.Second, d.Nodes[0], d.Nodes[1])
	if err := n3.Restart(); err != nil {
		t.Fatal(err)
	}
	if head := converge(t, d, 30*time.Second); head.Number != 8 {
		t.Fatalf("after the restart all nodes are at %d, want 8", head.Number)
	}
}

// A transaction sent to one node is gossiped, mined by another, and has a receipt on all.
func TestClusterTransaction(t *testing.T) {
	d := startNet(t, devnet.Options{N: 3})
	bob := client.AddressFor("bob")
	want := new(big.Int).Add(balance(t, d.Nodes[0], bob), client.Ether(3))
	tx := transfer(t, d.Nodes[2], "alice", 0, bob, 3)
	waitPending(t, d.Nodes[0], tx)
	mine(t, d.Nodes[0], 1)
	converge(t, d, 20*time.Second)
	for _, n := range d.Nodes {
		rc := receipt(t, n, tx)
		if rc == nil || !rc.Success() || rc.BlockNumber != 1 {
			t.Fatalf("%s: receipt %+v, want success in block 1", n.ID, rc)
		}
		if got := balance(t, n, bob); got.Cmp(want) != 0 {
			t.Fatalf("%s: bob's balance %s, want %s", n.ID, got, want)
		}
	}
}
