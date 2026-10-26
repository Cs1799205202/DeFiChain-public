package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/spec"
)

// ---- one node --------------------------------------------------------------------------

func (n *Node) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, n.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.net.http
	if path == "/admin/mine" {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s on %s: %s: %s", method, path, n.ID, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (n *Node) get(path string, out any) error        { return n.do(http.MethodGet, path, nil, out) }
func (n *Node) post(path string, body, out any) error { return n.do(http.MethodPost, path, body, out) }

// Status is GET /status.
func (n *Node) Status() (spec.Status, error) {
	var s spec.Status
	return s, n.get("/status", &s)
}

// Head is the node's canonical head.
func (n *Node) Head() (spec.BlockRef, error) {
	s, err := n.Status()
	return s.Head, err
}

// Mine mines k blocks on this node now (its auto-miner must be off).
func (n *Node) Mine(k int) ([]spec.BlockRef, error) {
	var res spec.MineResult
	err := n.post("/admin/mine", map[string]int{"blocks": k}, &res)
	if err == nil && res.Error != "" {
		err = fmt.Errorf("mine on %s: %s", n.ID, res.Error)
	}
	return res.Blocks, err
}

// SetMining turns the auto-miner on or off.
func (n *Node) SetMining(on bool) error {
	return n.post("/admin/miner", spec.MinerState{Mining: &on}, nil)
}

// SetHashrate caps the node's hashrate (0 = unlimited).
func (n *Node) SetHashrate(h float64) error {
	return n.post("/admin/miner", spec.MinerState{Hashrate: &h}, nil)
}

// NetConditions is the node's current fault injection.
func (n *Node) NetConditions() (spec.NetConditions, error) {
	var c spec.NetConditions
	return c, n.get("/admin/net", &c)
}

// SetNet replaces the node's fault injection.
func (n *Node) SetNet(c spec.NetConditions) error { return n.post("/admin/net", c, nil) }

// Block blocks (or unblocks) traffic between this node and the given peers, keeping the
// other fault injection settings.
func (n *Node) Block(peers []string) error {
	c, err := n.NetConditions()
	if err != nil {
		return err
	}
	c.Blocked = peers
	return n.SetNet(c)
}

// Chain lists the node's canonical blocks from..to (inclusive).
func (n *Node) Chain(from, to uint64) ([]spec.BlockInfo, error) {
	var out []spec.BlockInfo
	return out, n.get(fmt.Sprintf("/admin/chain?from=%d&to=%d", from, to), &out)
}

// Blocks lists every block the node stores, canonical or not.
func (n *Node) Blocks() ([]spec.BlockInfo, error) {
	var out []spec.BlockInfo
	return out, n.get("/admin/blocks", &out)
}

// Reorgs is GET /admin/reorgs.
func (n *Node) Reorgs() (spec.Reorgs, error) {
	var r spec.Reorgs
	return r, n.get("/admin/reorgs", &r)
}

// ---- the whole network -----------------------------------------------------------------

// Partition splits the network: nodes in different groups cannot talk to each other.
// Nodes not in any group are cut off from everyone.
func (d *Net) Partition(groups ...[]*Node) error {
	group := map[string]int{}
	for g, nodes := range groups {
		for _, n := range nodes {
			group[n.ID] = g + 1
		}
	}
	for _, n := range d.Alive() {
		var blocked []string
		for _, m := range d.Nodes {
			if m != n && (group[n.ID] == 0 || group[m.ID] != group[n.ID]) {
				blocked = append(blocked, m.ID)
			}
		}
		if err := n.Block(blocked); err != nil {
			return err
		}
	}
	return nil
}

// Heal removes every partition (delay, jitter and drop settings stay).
func (d *Net) Heal() error {
	for _, n := range d.Alive() {
		if err := n.Block(nil); err != nil {
			return err
		}
	}
	return nil
}

// SetNetAll applies the same fault injection to every live node.
func (d *Net) SetNetAll(c spec.NetConditions) error {
	for _, n := range d.Alive() {
		if err := n.SetNet(c); err != nil {
			return err
		}
	}
	return nil
}

// SetMiningAll turns auto-mining on or off on every live node.
func (d *Net) SetMiningAll(on bool) error {
	for _, n := range d.Alive() {
		if err := n.SetMining(on); err != nil {
			return err
		}
	}
	return nil
}

// Heads returns every given node's head (all live nodes if none given).
func (d *Net) Heads(nodes ...*Node) (map[string]spec.BlockRef, error) {
	if len(nodes) == 0 {
		nodes = d.Alive()
	}
	out := map[string]spec.BlockRef{}
	for _, n := range nodes {
		h, err := n.Head()
		if err != nil {
			return nil, err
		}
		out[n.ID] = h
	}
	return out, nil
}

// WaitConverged waits until the given nodes (all live nodes if none given) have the same
// head, and returns it.
func (d *Net) WaitConverged(ctx context.Context, nodes ...*Node) (spec.BlockRef, error) {
	var last map[string]spec.BlockRef
	for {
		heads, err := d.Heads(nodes...)
		if err == nil {
			last = heads
			var ref *spec.BlockRef
			same := true
			for _, h := range heads {
				if ref == nil {
					ref = &h
				} else if h.Hash != ref.Hash {
					same = false
				}
			}
			if same && ref != nil {
				return *ref, nil
			}
		}
		select {
		case <-ctx.Done():
			return spec.BlockRef{}, fmt.Errorf("nodes did not converge: %w; heads: %s", ctx.Err(), FormatHeads(last))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WaitHeight waits until every given node (all live nodes if none given) has a head at
// least `height`.
func (d *Net) WaitHeight(ctx context.Context, height uint64, nodes ...*Node) error {
	var last map[string]spec.BlockRef
	for {
		heads, err := d.Heads(nodes...)
		if err == nil {
			last = heads
			ok := true
			for _, h := range heads {
				ok = ok && h.Number >= height
			}
			if ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("height %d not reached: %w; heads: %s", height, ctx.Err(), FormatHeads(last))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WaitBlock waits until node n has block `hash` as a canonical block.
func (d *Net) WaitBlock(ctx context.Context, n *Node, number uint64, hash common.Hash) error {
	for {
		if blocks, err := n.Chain(number, number); err == nil && len(blocks) == 1 && blocks[0].Hash == hash {
			return nil
		}
		select {
		case <-ctx.Done():
			head, _ := n.Head()
			return fmt.Errorf("%s: block %d %s not canonical: %w (head %d %s)", n.ID, number,
				hash.TerminalString(), ctx.Err(), head.Number, head.Hash.TerminalString())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// FormatHeads prints heads compactly, sorted by node id.
func FormatHeads(heads map[string]spec.BlockRef) string {
	ids := make([]string, 0, len(heads))
	for id := range heads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var b strings.Builder
	for _, id := range ids {
		h := heads[id]
		fmt.Fprintf(&b, "%s=%d/%s(td %s) ", id, h.Number, h.Hash.TerminalString(), h.TD)
	}
	return strings.TrimSpace(b.String())
}
