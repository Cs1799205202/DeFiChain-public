// Package tests holds the multi-node tests: each one starts a devnet of real node processes
// (built from your code in ./cmd/node) and checks, from the outside, that they agree.
package tests

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"defichain/client"
	"defichain/devnet"
	"defichain/spec"
)

// startNet starts a devnet for one test (nodes do not mine unless asked) and stops it at the
// end. If the test fails, the last lines of every node's log are printed.
func startNet(t *testing.T, o devnet.Options) *devnet.Net {
	t.Helper()
	if o.Genesis == nil {
		o.Genesis = devnet.TestGenesis()
	}
	d, err := devnet.Start(o)
	if err != nil {
		t.Fatalf("start devnet: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, n := range d.Nodes {
				t.Logf("---- %s, last log lines ----\n%s", n.ID, n.LogTail(40))
			}
		}
		d.Close()
	})
	return d
}

func timeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// mine mines k blocks on n now.
func mine(t *testing.T, n *devnet.Node, k int) []spec.BlockRef {
	t.Helper()
	blocks, err := n.Mine(k)
	if err != nil {
		t.Fatalf("mine %d blocks on %s: %v", k, n.ID, err)
	}
	if len(blocks) != k {
		t.Fatalf("mine on %s: got %d blocks, want %d", n.ID, len(blocks), k)
	}
	return blocks
}

// converge waits until every live node has the same head and returns it.
func converge(t *testing.T, d *devnet.Net, within time.Duration, nodes ...*devnet.Node) spec.BlockRef {
	t.Helper()
	head, err := d.WaitConverged(timeout(t, within), nodes...)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// transfer signs a transfer of `eth` ETH from the dev account `from` (nonce `nonce`) to `to`
// and submits it to node n.
func transfer(t *testing.T, n *devnet.Node, from string, nonce uint64, to common.Address, eth int64) common.Hash {
	t.Helper()
	signer := types.LatestSignerForChainID(big.NewInt(int64(devnet.TestGenesis().Config.ChainID)))
	tx := types.MustSignNewTx(client.KeyFor(from), signer, &types.DynamicFeeTx{ChainID: signer.ChainID(), Nonce: nonce,
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(1e9), Gas: 21000, To: &to, Value: client.Ether(eth)})
	if err := client.NewRPC(n.URL).SendRaw(timeout(t, 10*time.Second), tx); err != nil {
		t.Fatalf("send transaction to %s: %v", n.ID, err)
	}
	return tx.Hash()
}

// waitPending waits until node n has transaction h in its pool (it was gossiped there).
func waitPending(t *testing.T, n *devnet.Node, h common.Hash) {
	t.Helper()
	ctx := timeout(t, 15*time.Second)
	rpc := client.NewRPC(n.URL)
	for {
		var tx map[string]any
		if err := rpc.Call(ctx, &tx, "eth_getTransactionByHash", h); err == nil && tx != nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("transaction %s did not reach %s's txpool", h.TerminalString(), n.ID)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func receipt(t *testing.T, n *devnet.Node, h common.Hash) *client.Receipt {
	t.Helper()
	rc, err := client.NewRPC(n.URL).Receipt(timeout(t, 10*time.Second), h)
	if err != nil {
		t.Fatalf("receipt from %s: %v", n.ID, err)
	}
	return rc
}

func balance(t *testing.T, n *devnet.Node, a common.Address) *big.Int {
	t.Helper()
	b, err := client.NewRPC(n.URL).Balance(timeout(t, 10*time.Second), a)
	if err != nil {
		t.Fatalf("balance from %s: %v", n.ID, err)
	}
	return b
}

func reorgs(t *testing.T, n *devnet.Node) int {
	t.Helper()
	r, err := n.Reorgs()
	if err != nil {
		t.Fatal(err)
	}
	return r.Count
}

func ids(nodes ...*devnet.Node) string {
	var s []string
	for _, n := range nodes {
		s = append(s, n.ID)
	}
	return strings.Join(s, ",")
}
