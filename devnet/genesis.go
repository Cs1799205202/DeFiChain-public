package devnet

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"defichain/client"
	"defichain/spec"
)

// DevAccounts are funded in every devnet genesis, 10,000 ETH each. Their private keys are
// client.KeyFor(name): public knowledge, for the lab only.
var DevAccounts = []string{"alice", "bob", "carol", "dave", "eve", "deployer"}

// MinerAddress is the coinbase of devnet node id (the address of KeyFor("miner-"+id)).
func MinerAddress(id string) string { return client.AddressFor("miner-" + id).Hex() }

// assumedHashrate is used to size the genesis difficulty of nodes with no hashrate cap.
const assumedHashrate = 1_000_000

// DevGenesis is a devnet genesis for n miners with the given hashrates (as in Options):
// the default rules, and a starting difficulty near what the network will settle at, so
// that blocks come at about the target rate from the start. extra adds allocations
// (address -> wei) on top of the dev accounts.
func DevGenesis(n int, hashrate []float64, extra map[common.Address]*big.Int) *spec.Genesis {
	cfg := spec.DefaultChainConfig()
	total := 0.0
	for i := 0; i < n; i++ {
		h := assumedHashrate * 1.0
		if len(hashrate) > 0 {
			h = hashrate[min(i, len(hashrate)-1)]
			if h <= 0 {
				h = assumedHashrate
			}
		}
		total += h
	}
	// With timestamps in whole seconds the Homestead rule settles at about 1.3x the
	// target interval (see the lab handout), i.e. difficulty ~ 1.3 * hashrate * target.
	diff := uint64(1.3 * total * float64(cfg.TargetBlockTime))
	cfg.MinimumDifficulty = max(diff/32, cfg.DifficultyBoundDivisor)
	diff = max(diff, cfg.MinimumDifficulty)
	g := &spec.Genesis{Config: cfg, Timestamp: uint64(time.Now().Unix()), Difficulty: diff, ExtraData: "defichain devnet",
		Alloc: map[string]spec.Account{}}
	for _, name := range DevAccounts {
		g.Alloc[client.AddressFor(name).Hex()] = spec.Account{Balance: spec.NewBig(client.Ether(10_000))}
	}
	for addr, wei := range extra {
		g.Alloc[addr.Hex()] = spec.Account{Balance: spec.NewBig(wei)}
	}
	return g
}

// TestGenesis is the genesis the cluster tests use: tiny difficulty (blocks mine in
// milliseconds on one core) that barely moves (divisor 2048), and a generous
// MaxFutureTime, so a test can mine many blocks in a row faster than one per second.
func TestGenesis() *spec.Genesis {
	cfg := spec.DefaultChainConfig()
	cfg.DifficultyBoundDivisor = 2048
	cfg.MinimumDifficulty = 2048
	cfg.MaxFutureTime = 3600
	g := &spec.Genesis{Config: cfg, Timestamp: uint64(time.Now().Unix()), Difficulty: 1 << 14, ExtraData: "defichain test",
		Alloc: map[string]spec.Account{}}
	for _, name := range DevAccounts {
		g.Alloc[client.AddressFor(name).Hex()] = spec.Account{Balance: spec.NewBig(client.Ether(10_000))}
	}
	return g
}
