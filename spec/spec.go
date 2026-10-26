// Package spec holds the formats shared by the node (chain/), the devnet launcher (devnet/),
// the simulator and the tests: chain parameters, the genesis block, a node's own settings,
// and the JSON returned by a node's admin API.
//
// It does not import anything else in this module, so tools that only launch nodes or talk
// to them over HTTP keep compiling while the node itself is still under construction.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ChainConfig is the consensus rules every node of one network must agree on. It is part
// of the genesis file, so two nodes with different rules have different genesis blocks
// and simply do not talk to each other.
type ChainConfig struct {
	ChainID uint64 `json:"chainId"`
	// TargetBlockTime (seconds) is the block interval the difficulty adjustment aims at.
	// Timestamps are whole seconds, so it must be at least 2 (see CalcDifficulty).
	TargetBlockTime uint64 `json:"targetBlockTime"`
	// DifficultyBoundDivisor sets how fast difficulty reacts: one block can raise it by
	// parent/divisor, or lower it by up to 99*parent/divisor. Ethereum used 2048.
	DifficultyBoundDivisor uint64 `json:"difficultyBoundDivisor"`
	// MinimumDifficulty is the floor below which difficulty never falls.
	MinimumDifficulty uint64 `json:"minimumDifficulty"`
	// MaxFutureTime (seconds): a block whose timestamp is further ahead of the local
	// clock than this is not accepted yet (ErrFutureBlock).
	MaxFutureTime uint64 `json:"maxFutureTime"`
	// GasLimit is the gas limit of every block.
	GasLimit uint64 `json:"gasLimit"`
	// BlockReward (wei) is credited to the coinbase of every block, after its transactions.
	BlockReward *Big `json:"blockReward"`
}

// Validate rejects parameters under which the difficulty rule cannot work.
func (c *ChainConfig) Validate() error {
	switch {
	case c.ChainID == 0:
		return errors.New("chainId must be set")
	case c.TargetBlockTime < 2:
		return errors.New("targetBlockTime must be >= 2 seconds (timestamps are whole seconds)")
	case c.DifficultyBoundDivisor == 0:
		return errors.New("difficultyBoundDivisor must be > 0")
	case c.MinimumDifficulty < c.DifficultyBoundDivisor:
		// Below the divisor, parent/divisor is 0 and the difficulty could never move again.
		return errors.New("minimumDifficulty must be >= difficultyBoundDivisor")
	case c.MaxFutureTime == 0:
		return errors.New("maxFutureTime must be > 0")
	case c.GasLimit < 1_000_000:
		return errors.New("gasLimit must be >= 1,000,000")
	case c.BlockReward == nil || c.BlockReward.Sign() < 0:
		return errors.New("blockReward must be set and non-negative")
	}
	return nil
}

// DefaultChainConfig is the devnet's rules: ~2-3 s blocks, fast-reacting difficulty.
func DefaultChainConfig() ChainConfig {
	return ChainConfig{
		ChainID:                4242,
		TargetBlockTime:        2,
		DifficultyBoundDivisor: 16,
		MinimumDifficulty:      1024,
		MaxFutureTime:          15,
		GasLimit:               30_000_000,
		BlockReward:            NewBig(new(big.Int).Mul(big.NewInt(2), big.NewInt(1e18))), // 2 ETH
	}
}

// Genesis describes block 0. Every node of a network must load the same file: the hash of
// the resulting genesis block identifies the network.
type Genesis struct {
	Config     ChainConfig        `json:"config"`
	Timestamp  uint64             `json:"timestamp"`
	Difficulty uint64             `json:"difficulty"`
	ExtraData  string             `json:"extraData,omitempty"` // free text, at most 32 bytes
	Alloc      map[string]Account `json:"alloc"`               // 0x address -> initial account
}

// Account is a genesis allocation.
type Account struct {
	Balance *Big `json:"balance"`
}

// Validate checks the genesis file for mistakes that would otherwise surface as a
// confusing "unknown parent" much later.
func (g *Genesis) Validate() error {
	if err := g.Config.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if g.Difficulty < g.Config.MinimumDifficulty {
		return fmt.Errorf("difficulty %d is below minimumDifficulty %d", g.Difficulty, g.Config.MinimumDifficulty)
	}
	if len(g.ExtraData) > 32 {
		return errors.New("extraData is longer than 32 bytes")
	}
	for addr, acc := range g.Alloc {
		if !common.IsHexAddress(addr) {
			return fmt.Errorf("alloc: %q is not an address", addr)
		}
		if acc.Balance == nil || acc.Balance.Sign() < 0 {
			return fmt.Errorf("alloc %s: balance must be set and non-negative", addr)
		}
	}
	return nil
}

// LoadGenesis reads and validates a genesis file.
func LoadGenesis(path string) (*Genesis, error) {
	var g Genesis
	if err := readJSON(path, &g); err != nil {
		return nil, err
	}
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &g, nil
}

// NodeConfig is one node's own settings (not consensus rules: nodes may differ).
type NodeConfig struct {
	ID     string `json:"id"`
	Listen string `json:"listen"` // host:port for JSON-RPC, p2p and admin, e.g. 127.0.0.1:28001
	// Peers are the nodes this one talks to directly: id -> base URL. A node only gossips
	// with its peers; with a sparse topology, blocks reach the rest by relay.
	Peers    map[string]string `json:"peers"`
	Coinbase string            `json:"coinbase"` // receives block rewards and tips (0x address)
	Mine     bool              `json:"mine"`     // start mining at boot
	// Hashrate caps mining speed in hashes per second (0 = as fast as one core can go).
	// Giving nodes different hashrates is how you model miners of different power.
	Hashrate float64 `json:"hashrate"`
	// Net is the initial network fault injection; change it at runtime via /admin/net.
	Net NetConditions `json:"net"`
	// Verbose logs every block received, not only the ones mined and reorgs.
	Verbose bool `json:"verbose,omitempty"`
	// UnsafeExpose allows listening on a non-loopback address. The admin API has no
	// authentication: anyone who can reach it can stop your miner or cut your peers.
	UnsafeExpose bool `json:"unsafeExpose,omitempty"`
}

// LoadNodeConfig reads a node config file.
func LoadNodeConfig(path string) (*NodeConfig, error) {
	var c NodeConfig
	if err := readJSON(path, &c); err != nil {
		return nil, err
	}
	switch {
	case c.ID == "":
		return nil, fmt.Errorf("%s: id is required", path)
	case c.Listen == "":
		return nil, fmt.Errorf("%s: listen is required", path)
	case c.Coinbase != "" && !common.IsHexAddress(c.Coinbase):
		return nil, fmt.Errorf("%s: coinbase %q is not an address", path, c.Coinbase)
	}
	return &c, nil
}

// NetConditions is a node's simulated network: which peers it cannot reach, and how slow
// or lossy its links are. It applies to everything the node sends and receives over p2p
// (blocks, transactions, sync requests), not to JSON-RPC or the admin API.
type NetConditions struct {
	Blocked []string `json:"blocked"` // peer ids this node cannot talk to, in either direction
	Delay   Duration `json:"delay"`   // added to every message this node sends
	Jitter  Duration `json:"jitter"`  // plus a uniform random extra delay in [0, jitter)
	Drop    float64  `json:"drop"`    // probability that a pushed block or tx is lost
}

// BlockRef identifies a block.
type BlockRef struct {
	Number uint64      `json:"number"`
	Hash   common.Hash `json:"hash"`
	TD     *Big        `json:"td"` // total difficulty from genesis up to and including this block
}

// Status is GET /status.
type Status struct {
	ID       string        `json:"id"`
	Genesis  common.Hash   `json:"genesis"`
	Head     BlockRef      `json:"head"`
	Peers    []string      `json:"peers"`
	Mining   bool          `json:"mining"`
	Hashrate float64       `json:"hashrate"` // configured cap, 0 = unlimited
	Coinbase string        `json:"coinbase"`
	TxPool   int           `json:"txpool"`
	Reorgs   int           `json:"reorgs"` // reorgs since start
	Blocks   int           `json:"blocks"` // all blocks stored, canonical or not
	Net      NetConditions `json:"net"`
	Uptime   float64       `json:"uptime"` // seconds
	PID      int           `json:"pid"`    // the node's process id
}

// ReorgEvent records one change of canonical chain that was not a simple extension.
type ReorgEvent struct {
	Time       time.Time `json:"time"`
	OldHead    BlockRef  `json:"oldHead"`
	NewHead    BlockRef  `json:"newHead"`
	Ancestor   BlockRef  `json:"ancestor"` // last block both chains share
	Removed    int       `json:"removed"`  // blocks that left the canonical chain
	Added      int       `json:"added"`    // blocks that joined it
	DroppedTxs int       `json:"droppedTxs"`
}

// MineResult is POST /admin/mine.
type MineResult struct {
	Blocks []BlockRef `json:"blocks"`
	Error  string     `json:"error,omitempty"`
}

// MinerState is GET/POST /admin/miner. In a POST, omitted fields are left unchanged.
type MinerState struct {
	Mining   *bool    `json:"mining,omitempty"`
	Hashrate *float64 `json:"hashrate,omitempty"` // hashes per second, 0 = unlimited
}

// BlockInfo describes a stored block (GET /admin/chain and /admin/blocks).
type BlockInfo struct {
	Number     uint64         `json:"number"`
	Hash       common.Hash    `json:"hash"`
	Parent     common.Hash    `json:"parent"`
	Miner      common.Address `json:"miner"`
	Time       uint64         `json:"time"`
	Difficulty *Big           `json:"difficulty"`
	TD         *Big           `json:"td"`
	Txs        int            `json:"txs"`
	GasUsed    uint64         `json:"gasUsed"`
	Canonical  bool           `json:"canonical"`
}

// Reorgs is GET /admin/reorgs.
type Reorgs struct {
	Count  int          `json:"count"`  // since the node started
	Recent []ReorgEvent `json:"recent"` // the latest ones, oldest first
}

// Big is a *big.Int that reads JSON numbers or strings (decimal or 0x-hex) and writes a
// decimal string, which JavaScript and Python can read without losing precision.
type Big big.Int

// NewBig wraps a copy of x (nil stays nil).
func NewBig(x *big.Int) *Big {
	if x == nil {
		return nil
	}
	return (*Big)(new(big.Int).Set(x))
}

// Int returns the value as a *big.Int (a copy; nil for nil).
func (b *Big) Int() *big.Int {
	if b == nil {
		return nil
	}
	return new(big.Int).Set((*big.Int)(b))
}

// Sign is the sign of b.
func (b *Big) Sign() int { return (*big.Int)(b).Sign() }

func (b *Big) String() string { return (*big.Int)(b).String() }

func (b Big) MarshalJSON() ([]byte, error) { return json.Marshal((*big.Int)(&b).String()) }

func (b *Big) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	x, ok := new(big.Int).SetString(s, 0) // base 0: decimal, or 0x-prefixed hex
	if !ok {
		return fmt.Errorf("not an integer: %s", data)
	}
	*b = Big(*x)
	return nil
}

// Duration is a time.Duration written as a Go duration string ("250ms", "2s"); a bare
// JSON number is read as milliseconds.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		var ms float64
		if err := json.Unmarshal(data, &ms); err != nil {
			return fmt.Errorf("duration: %s", data)
		}
		*d = Duration(ms * float64(time.Millisecond))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // a typo in a field name should not be silently ignored
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
