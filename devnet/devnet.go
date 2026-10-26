// Package devnet launches a local network of defichain nodes, each one a separate OS
// process (like separate machines), and drives it over HTTP: mine, partition, kill,
// restart, wait for convergence. cmd/devnet, cmd/sim and the cluster tests use it.
//
// It does not import the node's code (only the shared formats in spec/): it builds
// ./cmd/node with `go build` and runs the binary, so it keeps working while your node
// does not compile yet, and reports the build error instead.
package devnet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"defichain/spec"
)

// Options configures Start.
type Options struct {
	N        int    // number of nodes (default 3)
	Topology string // "mesh" (default: everyone peers with everyone), "line", "ring" or "star"
	// Hashrate per node in hashes/s (0 = unlimited). Shorter than N: the last value repeats.
	Hashrate []float64
	// Mine starts auto-mining on every node at boot (else mine with Net.Mine / /admin/mine).
	Mine bool
	// MineNodes, if set, overrides Mine per node (true = that node mines at boot).
	MineNodes []bool
	// Genesis to use; nil = DevGenesis(N, Hashrate, nil).
	Genesis *spec.Genesis
	// BasePort for node i is BasePort+i+1; 0 picks free ports (what tests want).
	BasePort int
	// Dir for genesis.json, nodeN.json and nodeN.log; "" = a temp dir removed by Close.
	Dir string
	// Bin is a prebuilt node binary; "" = build ./cmd/node of this module.
	Bin string
	// Net is every node's initial fault injection (delay, jitter, drop).
	Net spec.NetConditions
	// Verbose node logs.
	Verbose bool
	// Logs receives every node's output, line by line, prefixed (nil = only the log files).
	Logs io.Writer
}

// Net is a running devnet.
type Net struct {
	Genesis *spec.Genesis
	Nodes   []*Node
	Dir     string
	opts    Options
	bin     string
	tmp     bool
	http    *http.Client
	mu      sync.Mutex
}

// Node is one node process.
type Node struct {
	Index    int
	ID       string // n1, n2, ...
	URL      string // http://127.0.0.1:port: JSON-RPC, admin and p2p
	Port     int
	Coinbase string
	LogPath  string
	cfgPath  string
	net      *Net

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
	logf   *os.File
}

// NodeID is the id of node i: n1, n2, ...
func NodeID(i int) string { return fmt.Sprintf("n%d", i+1) }

// Start writes the configs, builds the node binary and starts every node.
func Start(o Options) (*Net, error) {
	if o.N <= 0 {
		o.N = 3
	}
	if o.Topology == "" {
		o.Topology = "mesh"
	}
	d := &Net{opts: o, Dir: o.Dir, http: &http.Client{Timeout: 30 * time.Second}}
	if d.Dir == "" {
		dir, err := os.MkdirTemp("", "defichain-devnet-*")
		if err != nil {
			return nil, err
		}
		d.Dir, d.tmp = dir, true
	} else if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return nil, err
	}
	d.Genesis = o.Genesis
	if d.Genesis == nil {
		d.Genesis = DevGenesis(o.N, o.Hashrate, nil)
	}
	if err := d.Genesis.Validate(); err != nil {
		return nil, fmt.Errorf("genesis: %w", err)
	}
	if err := writeJSON(filepath.Join(d.Dir, "genesis.json"), d.Genesis); err != nil {
		return nil, err
	}
	bin := o.Bin
	if bin == "" {
		var err error
		if bin, err = BuildNode(); err != nil {
			d.Close()
			return nil, err
		}
	}
	d.bin = bin
	for i := 0; i < o.N; i++ {
		d.Nodes = append(d.Nodes, &Node{Index: i, ID: NodeID(i), Coinbase: MinerAddress(NodeID(i)),
			LogPath: filepath.Join(d.Dir, NodeID(i)+".log"), cfgPath: filepath.Join(d.Dir, NodeID(i)+".json"), net: d})
	}
	// With free ports, another process may take a port between the moment it is picked and
	// the moment the node binds it: then pick new ports and start again.
	for attempt := 1; ; attempt++ {
		err := d.startAll()
		if err == nil {
			return d, nil
		}
		for _, n := range d.Nodes {
			n.Kill()
		}
		if o.BasePort != 0 || attempt == 3 || !errors.Is(err, errPortTaken) {
			d.Close()
			return nil, err
		}
		for _, n := range d.Nodes {
			os.Remove(n.LogPath)
		}
	}
}

// startAll assigns the ports, writes the configs and starts every node.
func (d *Net) startAll() error {
	for _, n := range d.Nodes {
		port := d.opts.BasePort + n.Index + 1
		if d.opts.BasePort == 0 {
			var err error
			if port, err = freePort(); err != nil {
				return err
			}
		}
		n.Port, n.URL = port, fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	for _, n := range d.Nodes {
		if err := d.writeConfig(n, d.peersOf(n.Index, len(d.Nodes))); err != nil {
			return err
		}
	}
	for _, n := range d.Nodes {
		if err := n.spawn(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, n := range d.Nodes {
		if err := n.waitUp(ctx); err != nil {
			return err
		}
	}
	return nil
}

// peersOf returns the peer ids of node i under the topology, for n nodes.
func (d *Net) peersOf(i, n int) []int {
	var out []int
	switch d.opts.Topology {
	case "line":
		if i > 0 {
			out = append(out, i-1)
		}
		if i < n-1 {
			out = append(out, i+1)
		}
	case "ring":
		if n > 1 {
			out = append(out, (i+n-1)%n)
		}
		if n > 2 {
			out = append(out, (i+1)%n)
		}
	case "star":
		if i == 0 {
			for j := 1; j < n; j++ {
				out = append(out, j)
			}
		} else {
			out = append(out, 0)
		}
	default: // mesh
		for j := 0; j < n; j++ {
			if j != i {
				out = append(out, j)
			}
		}
	}
	return out
}

func (d *Net) hashrate(i int) float64 {
	h := d.opts.Hashrate
	switch {
	case len(h) == 0:
		return 0
	case i < len(h):
		return h[i]
	}
	return h[len(h)-1]
}

func (d *Net) mines(i int) bool {
	if i < len(d.opts.MineNodes) {
		return d.opts.MineNodes[i]
	}
	return d.opts.Mine
}

func (d *Net) writeConfig(n *Node, peers []int) error {
	cfg := spec.NodeConfig{ID: n.ID, Listen: fmt.Sprintf("127.0.0.1:%d", n.Port), Peers: map[string]string{},
		Coinbase: n.Coinbase, Mine: d.mines(n.Index), Hashrate: d.hashrate(n.Index), Net: d.opts.Net, Verbose: d.opts.Verbose}
	for _, j := range peers {
		cfg.Peers[d.Nodes[j].ID] = d.Nodes[j].URL
	}
	return writeJSON(n.cfgPath, cfg)
}

// AddNode starts one more node (a late joiner), peered according to the topology (mesh:
// with everyone; otherwise with the last node), and tells its peers about it.
func (d *Net) AddNode(mine bool, hashrate float64) (*Node, error) {
	d.mu.Lock()
	i := len(d.Nodes)
	n := &Node{Index: i, ID: NodeID(i), Coinbase: MinerAddress(NodeID(i)), LogPath: filepath.Join(d.Dir, NodeID(i)+".log"),
		cfgPath: filepath.Join(d.Dir, NodeID(i)+".json"), net: d}
	d.Nodes = append(d.Nodes, n)
	d.opts.MineNodes = append(append([]bool{}, d.minesAll(i)...), mine)
	d.opts.Hashrate = append(append([]float64{}, d.hashrates(i)...), hashrate)
	var peers []int
	if d.opts.Topology == "mesh" {
		for j := 0; j < i; j++ {
			peers = append(peers, j)
		}
	} else if i > 0 {
		peers = []int{i - 1}
	}
	d.mu.Unlock()
	for attempt := 1; ; attempt++ {
		err := d.startNode(n, peers)
		if err == nil || d.opts.BasePort != 0 || attempt == 3 || !errors.Is(err, errPortTaken) {
			return n, err
		}
		n.Kill()
		os.Remove(n.LogPath)
	}
}

// startNode picks n's port, tells its peers about it and starts it.
func (d *Net) startNode(n *Node, peers []int) error {
	port := d.opts.BasePort + n.Index + 1
	if d.opts.BasePort == 0 {
		var err error
		if port, err = freePort(); err != nil {
			return err
		}
	}
	n.Port, n.URL = port, fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := d.writeConfig(n, peers); err != nil {
		return err
	}
	for _, j := range peers {
		if err := d.Nodes[j].post("/admin/peers", map[string]any{"add": map[string]string{n.ID: n.URL}}, nil); err != nil {
			return fmt.Errorf("tell %s about %s: %w", d.Nodes[j].ID, n.ID, err)
		}
	}
	if err := n.spawn(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return n.waitUp(ctx)
}

func (d *Net) minesAll(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = d.mines(i)
	}
	return out
}

func (d *Net) hashrates(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = d.hashrate(i)
	}
	return out
}

// URLs returns every node's URL.
func (d *Net) URLs() []string {
	out := make([]string, len(d.Nodes))
	for i, n := range d.Nodes {
		out[i] = n.URL
	}
	return out
}

// Alive returns the nodes whose process is running.
func (d *Net) Alive() []*Node {
	var out []*Node
	for _, n := range d.Nodes {
		if n.Running() {
			out = append(out, n)
		}
	}
	return out
}

// Close stops every node and, if the directory was temporary, removes it.
func (d *Net) Close() {
	var wg sync.WaitGroup
	for _, n := range d.Nodes {
		wg.Add(1)
		go func() { defer wg.Done(); n.Stop() }()
	}
	wg.Wait()
	if d.tmp {
		os.RemoveAll(d.Dir)
	}
}

// ---- processes -------------------------------------------------------------------------

func (n *Node) spawn() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd != nil {
		return nil
	}
	logf, err := os.OpenFile(n.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	var out io.Writer = logf
	if n.net.opts.Logs != nil {
		out = io.MultiWriter(logf, n.net.opts.Logs)
	}
	cmd := exec.Command(n.net.bin, "-genesis", filepath.Join(n.net.Dir, "genesis.json"), "-config", n.cfgPath, "-parent-watch")
	cmd.Stdout, cmd.Stderr = out, out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		logf.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("start %s: %w", n.ID, err)
	}
	n.cmd, n.stdin, n.logf, n.exited = cmd, stdin, logf, make(chan struct{})
	go func(cmd *exec.Cmd, exited chan struct{}) {
		cmd.Wait() //nolint:errcheck
		close(exited)
	}(cmd, n.exited)
	return nil
}

// waitUp waits until the node answers /status (or its process exits). The answer must
// come from the node's own process: with free ports, another process may hold the port.
func (n *Node) waitUp(ctx context.Context) error {
	poll := &http.Client{Timeout: 2 * time.Second}
	for {
		n.mu.Lock()
		exited, cmd := n.exited, n.cmd
		n.mu.Unlock()
		var s spec.Status
		if resp, err := poll.Get(n.URL + "/status"); err == nil {
			err = json.NewDecoder(resp.Body).Decode(&s)
			resp.Body.Close()
			if err == nil && cmd != nil && s.PID == cmd.Process.Pid {
				return nil
			}
		}
		select {
		case <-exited:
			tail := n.LogTail(30)
			if portTaken(tail) {
				return fmt.Errorf("node %s: %w:\n%s", n.ID, errPortTaken, tail)
			}
			return fmt.Errorf("node %s exited during startup:\n%s", n.ID, tail)
		case <-ctx.Done():
			return fmt.Errorf("node %s did not come up: %w\n%s", n.ID, ctx.Err(), n.LogTail(30))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Running reports whether the node's process is alive.
func (n *Node) Running() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return false
	}
	select {
	case <-n.exited:
		return false
	default:
		return true
	}
}

// Kill crashes the node (SIGKILL: no clean shutdown). It loses all its state: nodes keep
// nothing on disk, so when restarted it syncs from genesis again.
func (n *Node) Kill() {
	n.mu.Lock()
	cmd, exited := n.cmd, n.exited
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Kill() //nolint:errcheck
	<-exited
	n.cleanup()
}

// Stop shuts the node down cleanly (SIGINT), killing it if it does not exit in 5 s.
func (n *Node) Stop() {
	n.mu.Lock()
	cmd, exited := n.cmd, n.exited
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	if runtime.GOOS == "windows" || cmd.Process.Signal(os.Interrupt) != nil {
		cmd.Process.Kill() //nolint:errcheck
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		<-exited
	}
	n.cleanup()
}

func (n *Node) cleanup() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stdin != nil {
		n.stdin.Close()
	}
	if n.logf != nil {
		n.logf.Close()
	}
	n.cmd, n.stdin, n.logf = nil, nil, nil
}

// Restart starts a stopped or killed node again, with the same config, and waits until
// it answers.
func (n *Node) Restart() error {
	if err := n.spawn(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return n.waitUp(ctx)
}

// LogTail returns the last lines of the node's log.
func (n *Node) LogTail(lines int) string {
	data, err := os.ReadFile(n.LogPath)
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// ---- building the node -----------------------------------------------------------------

var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

// BuildNode compiles ./cmd/node of this module (once per process) and returns the path.
func BuildNode() (string, error) {
	buildOnce.Do(func() {
		root, err := ModuleRoot()
		if err != nil {
			buildErr = err
			return
		}
		sum := sha256.Sum256([]byte(root))
		dir := filepath.Join(os.TempDir(), "defichain-bin-"+hex.EncodeToString(sum[:6]))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "defichain-node")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/node")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/node failed (does your code compile?):\n%s", out)
			return
		}
		buildBin = bin
	})
	return buildBin, buildErr
}

// ModuleRoot finds the directory of this module's go.mod.
func ModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if ok {
		if root := filepath.Dir(filepath.Dir(file)); fileExists(filepath.Join(root, "go.mod")) {
			return root, nil
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("cannot find go.mod: run from inside the repository")
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// errPortTaken: a node could not bind its port (another process took it).
var errPortTaken = errors.New("port already in use")

func portTaken(log string) bool {
	for _, s := range []string{"address already in use", "Only one usage of each socket address", "bind: An attempt was made"} {
		if strings.Contains(log, s) {
			return true
		}
	}
	return false
}

var (
	portsMu   sync.Mutex
	portsUsed = map[int]bool{}
)

// freePort picks a port that is free now. It avoids the OS's ephemeral range (where the
// kernel also picks the source ports of outgoing connections, so a port found free there
// can be taken again at once) and never gives the same port twice in this process.
func freePort() (int, error) {
	portsMu.Lock()
	defer portsMu.Unlock()
	for try := 0; try < 200; try++ {
		port := 20000 + rand.Intn(8000) // below devnet's 28000 and Linux's ephemeral 32768+
		if portsUsed[port] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		ln.Close()
		portsUsed[port] = true
		return port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
