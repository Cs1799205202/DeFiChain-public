// Command devnet starts a local defichain network: N nodes, each a separate process, with
// genesis, configs and logs in one directory, and an interactive console to mine, cut the
// network, kill and restart nodes.
//
//	go run ./cmd/devnet                         # 3 mining nodes on ports 28001-28003
//	go run ./cmd/devnet -n 4 -hashrate 300000,100000,100000,100000
//	go run ./cmd/devnet -mine=false             # mine by hand: "mine n1 5"
//	go run ./cmd/devnet -topology line -delay 300ms -jitter 300ms
//
// Type "help" at the prompt for the console commands.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"defichain/client"
	"defichain/devnet"
	"defichain/spec"
)

func main() {
	n := flag.Int("n", 3, "number of nodes")
	topology := flag.String("topology", "mesh", "mesh, line, ring or star")
	hashrate := flag.String("hashrate", "200000", "hashrate cap per node in H/s, comma-separated (last value repeats; 0 = unlimited)")
	mine := flag.Bool("mine", true, "start auto-mining on every node")
	port := flag.Int("port", 28000, "node i listens on port+i")
	dir := flag.String("dir", "devnet-data", "directory for genesis.json, node configs and logs (cleared at start)")
	delay := flag.Duration("delay", 0, "p2p delay added to every message")
	jitter := flag.Duration("jitter", 0, "extra random p2p delay in [0, jitter)")
	drop := flag.Float64("drop", 0, "probability that a pushed block or transaction is lost")
	quiet := flag.Bool("quiet", false, "do not print node logs here (they are still in the log files)")
	verbose := flag.Bool("verbose", false, "nodes log every block they receive")
	easy := flag.Bool("easy", false, "tiny, nearly constant difficulty: manual blocks mine at once and every block weighs about the same (use with -mine=false)")
	flag.Parse()

	rates, err := parseRates(*hashrate)
	if err != nil {
		fail(err)
	}
	if err := clearDir(*dir); err != nil {
		fail(err)
	}
	opts := devnet.Options{N: *n, Topology: *topology, Hashrate: rates, Mine: *mine, BasePort: *port, Dir: *dir,
		Verbose: *verbose, Net: spec.NetConditions{Delay: spec.Duration(*delay), Jitter: spec.Duration(*jitter), Drop: *drop}}
	if *easy {
		opts.Genesis = devnet.TestGenesis()
		if *mine {
			fmt.Println("warning: -easy with auto-mining produces blocks many times a second; you probably want -mine=false")
		}
	}
	if !*quiet {
		opts.Logs = os.Stdout
	}
	fmt.Println("building and starting nodes...")
	d, err := devnet.Start(opts)
	if err != nil {
		fail(err)
	}
	defer d.Close()
	banner(d)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-sig:
			fmt.Println("\nstopping nodes...")
			return
		case line, ok := <-lines:
			if !ok { // stdin closed (not a terminal): just run until interrupted
				lines = nil
				continue
			}
			if quit := run(d, strings.Fields(line)); quit {
				fmt.Println("stopping nodes...")
				return
			}
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "devnet:", err)
	os.Exit(1)
}

func parseRates(s string) ([]float64, error) {
	var out []float64
	for _, f := range strings.Split(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil {
			return nil, fmt.Errorf("bad -hashrate %q", s)
		}
		out = append(out, v)
	}
	return out, nil
}

// clearDir removes the files a previous run left (genesis, configs, logs).
func clearDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, pat := range []string{"genesis.json", "n*.json", "n*.log"} {
		files, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, f := range files {
			os.Remove(f)
		}
	}
	return nil
}

func banner(d *devnet.Net) {
	g := d.Genesis
	fmt.Printf("\n=== defichain devnet: %d nodes, chain id %d, target block time %ds ===\n", len(d.Nodes), g.Config.ChainID, g.Config.TargetBlockTime)
	fmt.Printf("files and logs: %s\n\n", d.Dir)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "node\tJSON-RPC / admin URL\tcoinbase")
	for _, n := range d.Nodes {
		fmt.Fprintf(w, "%s\t%s\t%s\n", n.ID, n.URL, n.Coinbase)
	}
	w.Flush()
	fmt.Println("\nfunded dev accounts (10000 ETH each; the keys are public, for this devnet only):")
	w = tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	for _, name := range devnet.DevAccounts {
		key := client.KeyFor(name)
		fmt.Fprintf(w, "  %s\t%s\t%s\n", name, client.AddressOf(key).Hex(), client.KeyHex(key))
	}
	w.Flush()
	fmt.Printf(`
try:  curl -s %[1]s/status
      curl -s -X POST %[1]s -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}'
      cast block-number --rpc-url %[1]s      (Foundry)
MetaMask: add network, RPC URL %[1]s, chain id %[2]d, symbol ETH; import alice's key.
Type "help" for console commands, Ctrl-C to stop.

`, d.Nodes[0].URL, g.Config.ChainID)
}

const help = `commands (nodes are n1, n2, ...; "all" for every node where it makes sense):
  status | s                       heads, peers, mining, txpool, reorgs of every node
  mine <node> [k]                  mine k blocks now on <node> (its auto-miner must be off)
  miner <node|all> on|off          start/stop auto-mining
  hashrate <node|all> <h/s>        cap a node's hashrate (0 = unlimited)
  partition n1,n2 n3,n4            split the network into groups that cannot talk
  isolate <node>                   cut one node off from everyone
  heal                             remove all partitions
  net <node|all> [delay=200ms] [jitter=100ms] [drop=0.1]    set p2p delay/loss (omitted = 0)
  kill <node> | restart <node>     crash a node (it loses its chain) / start it again
  add [mine]                       start one more node, which syncs from the others
  chain <node> [k]                 the node's last k canonical blocks (default 10)
  reorgs <node>                    the node's reorg history
  quit | exit`

// run executes one console command and reports whether to quit.
func run(d *devnet.Net, args []string) bool {
	if len(args) == 0 {
		return false
	}
	if err := command(d, args); err != nil {
		fmt.Println("error:", err)
	}
	return args[0] == "quit" || args[0] == "exit"
}

func command(d *devnet.Net, args []string) error {
	nodes := func(arg string) ([]*devnet.Node, error) {
		if arg == "all" {
			return d.Alive(), nil
		}
		var out []*devnet.Node
		for _, id := range strings.Split(arg, ",") {
			found := false
			for _, n := range d.Nodes {
				if n.ID == id {
					out, found = append(out, n), true
				}
			}
			if !found {
				return nil, fmt.Errorf("no node %q", id)
			}
		}
		return out, nil
	}
	need := func(k int) error {
		if len(args) < k+1 {
			return fmt.Errorf("%s needs %d argument(s); type help", args[0], k)
		}
		return nil
	}
	switch args[0] {
	case "help", "h", "?":
		fmt.Println(help)
	case "quit", "exit":
	case "status", "s":
		status(d, os.Stdout)
	case "mine":
		if err := need(1); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		k := 1
		if len(args) > 2 {
			k, _ = strconv.Atoi(args[2])
		}
		for _, n := range ns {
			blocks, err := n.Mine(k)
			if err != nil {
				return err
			}
			for _, b := range blocks {
				fmt.Printf("%s mined %d %s\n", n.ID, b.Number, b.Hash.TerminalString())
			}
		}
	case "miner":
		if err := need(2); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		for _, n := range ns {
			if err := n.SetMining(args[2] == "on"); err != nil {
				return err
			}
		}
	case "hashrate":
		if err := need(2); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		h, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if err := n.SetHashrate(h); err != nil {
				return err
			}
		}
	case "partition":
		if err := need(2); err != nil {
			return err
		}
		var groups [][]*devnet.Node
		for _, g := range args[1:] {
			ns, err := nodes(g)
			if err != nil {
				return err
			}
			groups = append(groups, ns)
		}
		return d.Partition(groups...)
	case "isolate":
		if err := need(1); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		var rest []*devnet.Node
		for _, n := range d.Nodes {
			if n != ns[0] {
				rest = append(rest, n)
			}
		}
		return d.Partition(ns[:1], rest)
	case "heal":
		return d.Heal()
	case "net":
		if err := need(1); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		for _, n := range ns {
			c, err := n.NetConditions()
			if err != nil {
				return err
			}
			c.Delay, c.Jitter, c.Drop = 0, 0, 0
			for _, kv := range args[2:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "delay", "jitter":
					dur, err := time.ParseDuration(v)
					if err != nil {
						return err
					}
					if k == "delay" {
						c.Delay = spec.Duration(dur)
					} else {
						c.Jitter = spec.Duration(dur)
					}
				case "drop":
					if c.Drop, err = strconv.ParseFloat(v, 64); err != nil {
						return err
					}
				default:
					return fmt.Errorf("unknown setting %q", k)
				}
			}
			if err := n.SetNet(c); err != nil {
				return err
			}
		}
	case "kill", "restart":
		if err := need(1); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		for _, n := range ns {
			if args[0] == "kill" {
				n.Kill()
				fmt.Printf("%s killed\n", n.ID)
			} else if err := n.Restart(); err != nil {
				return err
			}
		}
	case "add":
		h := 0.0
		if s, err := d.Nodes[0].Status(); err == nil {
			h = s.Hashrate // same power as n1
		}
		n, err := d.AddNode(len(args) > 1 && args[1] == "mine", h)
		if err != nil {
			return err
		}
		fmt.Printf("%s started at %s\n", n.ID, n.URL)
	case "chain":
		if err := need(1); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		k := uint64(10)
		if len(args) > 2 {
			k, _ = strconv.ParseUint(args[2], 10, 64)
		}
		head, err := ns[0].Head()
		if err != nil {
			return err
		}
		from := uint64(0)
		if head.Number >= k {
			from = head.Number - k + 1
		}
		blocks, err := ns[0].Chain(from, head.Number)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "number\thash\tminer\ttime\tdifficulty\ttxs")
		for _, b := range blocks {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%d\n", b.Number, b.Hash.TerminalString(), minerName(d, b.Miner.Hex()),
				time.Unix(int64(b.Time), 0).Format("15:04:05"), b.Difficulty, b.Txs)
		}
		w.Flush()
	case "reorgs":
		if err := need(1); err != nil {
			return err
		}
		ns, err := nodes(args[1])
		if err != nil {
			return err
		}
		r, err := ns[0].Reorgs()
		if err != nil {
			return err
		}
		fmt.Printf("%s: %d reorgs\n", ns[0].ID, r.Count)
		for _, e := range r.Recent {
			fmt.Printf("  %s depth %d: %d %s -> %d %s (ancestor %d), %d txs back to the pool\n", e.Time.Format("15:04:05"),
				e.Removed, e.OldHead.Number, e.OldHead.Hash.TerminalString(), e.NewHead.Number, e.NewHead.Hash.TerminalString(),
				e.Ancestor.Number, e.DroppedTxs)
		}
	default:
		return fmt.Errorf("unknown command %q; type help", args[0])
	}
	return nil
}

func minerName(d *devnet.Net, addr string) string {
	for _, n := range d.Nodes {
		if strings.EqualFold(n.Coinbase, addr) {
			return n.ID
		}
	}
	return addr[:10]
}

func status(d *devnet.Net, out io.Writer) {
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "node\thead\thash\ttotal difficulty\tpeers\tmining\thashrate\ttxpool\treorgs\tblocks\tblocked")
	for _, n := range d.Nodes {
		s, err := n.Status()
		if err != nil {
			state := "down"
			if n.Running() {
				state = "not answering"
			}
			fmt.Fprintf(w, "%s\t(%s)\n", n.ID, state)
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%v\t%s\t%d\t%d\t%d\t%s\n", n.ID, s.Head.Number, s.Head.Hash.TerminalString(), s.Head.TD,
			len(s.Peers), s.Mining, rate(s.Hashrate), s.TxPool, s.Reorgs, s.Blocks, strings.Join(s.Net.Blocked, ","))
	}
	w.Flush()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if head, err := d.WaitConverged(ctx); err == nil {
		fmt.Fprintf(out, "all nodes agree on head %d\n", head.Number)
	} else {
		fmt.Fprintln(out, "nodes disagree on the head")
	}
}

func rate(h float64) string {
	switch {
	case h <= 0:
		return "unlimited"
	case h >= 1e6:
		return fmt.Sprintf("%.1fM", h/1e6)
	case h >= 1e3:
		return fmt.Sprintf("%.0fk", h/1e3)
	}
	return fmt.Sprintf("%.0f", h)
}
