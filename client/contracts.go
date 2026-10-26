package client

import (
	"crypto/ecdsa"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// KeyFor derives a private key from a name, so that simulations get stable, readable
// accounts ("alice", "arb", "llm1"). Anyone who knows the name knows the key: fine for a
// lab devnet, never for real money.
func KeyFor(name string) *ecdsa.PrivateKey {
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("defichain-key:" + name)))
	if err != nil {
		panic(fmt.Sprintf("key for %q: %v", name, err))
	}
	return key
}

// AddressOf is the account address of a key.
func AddressOf(key *ecdsa.PrivateKey) common.Address { return crypto.PubkeyToAddress(key.PublicKey) }

// AddressFor is AddressOf(KeyFor(name)).
func AddressFor(name string) common.Address { return AddressOf(KeyFor(name)) }

// KeyFromHex parses a hex private key (with or without 0x).
func KeyFromHex(s string) (*ecdsa.PrivateKey, error) {
	return crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
}

// KeyHex formats a private key as 0x-prefixed hex (what MetaMask's "import account" wants).
func KeyHex(key *ecdsa.PrivateKey) string { return "0x" + common.Bytes2Hex(crypto.FromECDSA(key)) }

// Contract is a compiled contract: its ABI and creation bytecode.
type Contract struct {
	Name    string
	ABI     abi.ABI
	ABIJSON string // the ABI as solc wrote it (what web3.py and ethers.js want)
	Bin     []byte
}

// artifacts holds the course contracts compiled by `make contracts` (solc --abi --bin).
//
//go:embed artifacts/*.abi artifacts/*.bin
var artifacts embed.FS

// Artifact loads a contract compiled into client/artifacts/ (Name.abi and Name.bin). To
// add your own contract: put it in contracts/, add it to CONTRACTS in the Makefile, run
// `make contracts`, and load it with client.Artifact("YourContract").
func Artifact(name string) (Contract, error) {
	abiJSON, err := artifacts.ReadFile("artifacts/" + name + ".abi")
	if err != nil {
		return Contract{}, fmt.Errorf("no compiled contract %q in client/artifacts (run make contracts)", name)
	}
	bin, _ := artifacts.ReadFile("artifacts/" + name + ".bin")
	return NewContract(name, string(abiJSON), string(bin))
}

func mustArtifact(name string) Contract {
	c, err := Artifact(name)
	if err != nil {
		panic(err)
	}
	return c
}

// The course contracts (contracts/*.sol).
var (
	TokenContract      = mustArtifact("Token")
	AMMContract        = mustArtifact("AMM")
	LendingContract    = mustArtifact("Lending")
	FeedOracleContract = mustArtifact("FeedOracle")
	AMMOracleContract  = mustArtifact("AMMOracle")
)

// NewContract builds a Contract from an ABI (JSON) and creation bytecode (hex), e.g. the
// output of `solc --abi --bin`. Bytecode may be empty if you only call a deployed contract.
func NewContract(name, abiJSON, binHex string) (Contract, error) {
	parsed, err := abi.JSON(strings.NewReader(abiJSON))
	if err != nil {
		return Contract{}, fmt.Errorf("%s ABI: %w", name, err)
	}
	return Contract{Name: name, ABI: parsed, ABIJSON: abiJSON, Bin: common.FromHex(strings.TrimSpace(binHex))}, nil
}

// LoadArtifact reads a Foundry artifact (out/X.sol/X.json) or a solc --combined-json
// style file with "abi" and "bytecode"/"bin" fields.
func LoadArtifact(path string) (Contract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Contract{}, err
	}
	var art struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode json.RawMessage `json:"bytecode"`
		Bin      string          `json:"bin"`
	}
	if err := json.Unmarshal(data, &art); err != nil {
		return Contract{}, fmt.Errorf("%s: %w", path, err)
	}
	bin := art.Bin
	if len(art.Bytecode) > 0 {
		var obj struct {
			Object string `json:"object"`
		}
		if json.Unmarshal(art.Bytecode, &obj) == nil && obj.Object != "" {
			bin = obj.Object
		} else {
			json.Unmarshal(art.Bytecode, &bin) //nolint:errcheck // plain string form
		}
	}
	name := strings.TrimSuffix(path[strings.LastIndexAny(path, `/\`)+1:], ".json")
	return NewContract(name, string(art.ABI), bin)
}

// LoadSolc reads the X.abi and X.bin files written by `solc --abi --bin -o dir`.
func LoadSolc(dir, name string) (Contract, error) {
	abiJSON, err := os.ReadFile(dir + "/" + name + ".abi")
	if err != nil {
		return Contract{}, err
	}
	bin, _ := os.ReadFile(dir + "/" + name + ".bin")
	return NewContract(name, string(abiJSON), string(bin))
}

// DeployData is the creation bytecode followed by the ABI-encoded constructor arguments.
func (c Contract) DeployData(args ...any) ([]byte, error) {
	if len(c.Bin) == 0 {
		return nil, fmt.Errorf("%s: no bytecode to deploy", c.Name)
	}
	packed, err := c.ABI.Pack("", args...)
	if err != nil {
		return nil, fmt.Errorf("%s constructor: %w", c.Name, err)
	}
	return append(append([]byte{}, c.Bin...), packed...), nil
}

// Calldata ABI-encodes a method call.
func (c Contract) Calldata(method string, args ...any) ([]byte, error) {
	data, err := c.ABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", c.Name, method, err)
	}
	return data, nil
}

// Decode unpacks a method's return data.
func (c Contract) Decode(method string, ret []byte) ([]any, error) {
	return c.ABI.Unpack(method, ret)
}

// ErrOtherEvent: the log is not the event you asked to decode.
var ErrOtherEvent = errors.New("log is a different event")

// DecodeEvent decodes a log as the named event: field name -> value (indexed fields too).
func (c Contract) DecodeEvent(name string, l *types.Log) (map[string]any, error) {
	ev, ok := c.ABI.Events[name]
	if !ok {
		return nil, fmt.Errorf("%s has no event %s", c.Name, name)
	}
	if len(l.Topics) == 0 || l.Topics[0] != ev.ID {
		return nil, ErrOtherEvent
	}
	out := map[string]any{}
	if len(l.Data) > 0 {
		if err := c.ABI.UnpackIntoMap(out, name, l.Data); err != nil {
			return nil, err
		}
	}
	var indexed abi.Arguments
	for _, arg := range ev.Inputs {
		if arg.Indexed {
			indexed = append(indexed, arg)
		}
	}
	if err := abi.ParseTopicsIntoMap(out, indexed, l.Topics[1:]); err != nil {
		return nil, err
	}
	return out, nil
}

// Events returns every log in logs that is event `name` emitted by addr (any address if
// addr is the zero address), decoded.
func (c Contract) Events(logs []*types.Log, addr common.Address, name string) []map[string]any {
	var out []map[string]any
	for _, l := range logs {
		if addr != (common.Address{}) && l.Address != addr {
			continue
		}
		if ev, err := c.DecodeEvent(name, l); err == nil {
			out = append(out, ev)
		}
	}
	return out
}

// EventTopic is the topic0 of an event, for log filters.
func (c Contract) EventTopic(name string) common.Hash { return c.ABI.Events[name].ID }
