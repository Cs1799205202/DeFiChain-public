// Package client is the Go SDK for talking to a defichain network (or any Ethereum JSON-RPC
// node): accounts and signing, contract ABIs, transactions with receipts and confirmations.
// It only uses standard JSON-RPC, so it does not depend on the node's code at all.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// RPC is a JSON-RPC client over one or more node URLs. It sticks to one node (so that
// what you read is consistent with what you sent) and fails over to the next one when that
// node does not answer.
type RPC struct {
	urls []string
	http *http.Client

	mu      sync.Mutex
	current int
	id      int64
}

// NewRPC creates a client for the given node URLs (e.g. http://127.0.0.1:28001).
func NewRPC(urls ...string) *RPC {
	return &RPC{urls: urls, http: &http.Client{Timeout: 15 * time.Second}}
}

// URLs returns the node URLs.
func (r *RPC) URLs() []string { return r.urls }

// Use makes node i the preferred one.
func (r *RPC) Use(i int) {
	r.mu.Lock()
	r.current = i % len(r.urls)
	r.mu.Unlock()
}

// Error is an error returned by the node (as opposed to a network failure).
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Call invokes method with params and decodes the result into out (which may be nil).
func (r *RPC) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	r.mu.Lock()
	r.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": r.id, "method": method, "params": params})
	start := r.current
	r.mu.Unlock()
	var lastErr error
	for i := 0; i < len(r.urls); i++ {
		idx := (start + i) % len(r.urls)
		res, err := r.post(ctx, r.urls[idx], body)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue // the node is down: try the next one
		}
		if i > 0 {
			r.Use(idx)
		}
		if res.Error != nil {
			return res.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(res.Result, out)
	}
	return fmt.Errorf("%s: no node answered: %w", method, lastErr)
}

type rpcResult struct {
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

func (r *RPC) post(ctx context.Context, url string, body []byte) (*rpcResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %s", url, resp.Status)
	}
	var res rpcResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return &res, nil
}

// ---- typed helpers ---------------------------------------------------------------------

// ChainID is eth_chainId.
func (r *RPC) ChainID(ctx context.Context) (*big.Int, error) {
	var v hexutil.Big
	err := r.Call(ctx, &v, "eth_chainId")
	return (*big.Int)(&v), err
}

// BlockNumber is the number of the canonical head.
func (r *RPC) BlockNumber(ctx context.Context) (uint64, error) {
	var v hexutil.Uint64
	err := r.Call(ctx, &v, "eth_blockNumber")
	return uint64(v), err
}

// Balance is an account's ETH balance (wei) at the head.
func (r *RPC) Balance(ctx context.Context, addr common.Address) (*big.Int, error) {
	var v hexutil.Big
	err := r.Call(ctx, &v, "eth_getBalance", addr, "latest")
	return (*big.Int)(&v), err
}

// Nonce is the account's nonce at the head ("latest") or including the txpool ("pending").
func (r *RPC) Nonce(ctx context.Context, addr common.Address, block string) (uint64, error) {
	var v hexutil.Uint64
	err := r.Call(ctx, &v, "eth_getTransactionCount", addr, block)
	return uint64(v), err
}

// CallArgs is a call or transaction object.
type CallArgs struct {
	From  common.Address  `json:"from"`
	To    *common.Address `json:"to,omitempty"`
	Data  hexutil.Bytes   `json:"data,omitempty"`
	Value *hexutil.Big    `json:"value,omitempty"`
	Gas   *hexutil.Uint64 `json:"gas,omitempty"`
}

// EthCall runs a read-only call at the head. A revert is returned as a *RevertError.
func (r *RPC) EthCall(ctx context.Context, args CallArgs) ([]byte, error) {
	var out hexutil.Bytes
	if err := r.Call(ctx, &out, "eth_call", args, "latest"); err != nil {
		return nil, asRevert(err)
	}
	return out, nil
}

// EthCallAt is EthCall on the state after canonical block n (historical state).
func (r *RPC) EthCallAt(ctx context.Context, args CallArgs, n uint64) ([]byte, error) {
	var out hexutil.Bytes
	if err := r.Call(ctx, &out, "eth_call", args, hexutil.EncodeUint64(n)); err != nil {
		return nil, asRevert(err)
	}
	return out, nil
}

// EstimateGas asks the node how much gas the call needs. A revert is a *RevertError.
func (r *RPC) EstimateGas(ctx context.Context, args CallArgs) (uint64, error) {
	var v hexutil.Uint64
	if err := r.Call(ctx, &v, "eth_estimateGas", args, "latest"); err != nil {
		return 0, asRevert(err)
	}
	return uint64(v), nil
}

// SendRaw submits a signed transaction.
func (r *RPC) SendRaw(ctx context.Context, tx *types.Transaction) error {
	raw, err := tx.MarshalBinary()
	if err != nil {
		return err
	}
	return r.Call(ctx, nil, "eth_sendRawTransaction", hexutil.Bytes(raw))
}

// Receipt is a transaction receipt.
type Receipt struct {
	TxHash          common.Hash     `json:"transactionHash"`
	BlockHash       common.Hash     `json:"blockHash"`
	BlockNumber     hexutil.Uint64  `json:"blockNumber"`
	Index           hexutil.Uint64  `json:"transactionIndex"`
	From            common.Address  `json:"from"`
	To              *common.Address `json:"to"`
	Status          hexutil.Uint64  `json:"status"`
	GasUsed         hexutil.Uint64  `json:"gasUsed"`
	ContractAddress *common.Address `json:"contractAddress"`
	Logs            []*types.Log    `json:"logs"`
	// RevertReason is a defichain extension: why a failed transaction reverted.
	RevertReason string `json:"revertReason"`
}

// Success reports whether the transaction executed without reverting.
func (r *Receipt) Success() bool { return r.Status == 1 }

// Receipt returns a transaction's receipt, or nil while it is not on the canonical chain.
func (r *RPC) Receipt(ctx context.Context, hash common.Hash) (*Receipt, error) {
	var rc *Receipt
	err := r.Call(ctx, &rc, "eth_getTransactionReceipt", hash)
	return rc, err
}

// Block is the part of a block header agents usually need.
type Block struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	Time       hexutil.Uint64 `json:"timestamp"`
	Miner      common.Address `json:"miner"`
	Difficulty *hexutil.Big   `json:"difficulty"`
	TD         *hexutil.Big   `json:"totalDifficulty"`
	GasUsed    hexutil.Uint64 `json:"gasUsed"`
	Txs        []common.Hash  `json:"transactions"`
}

// BlockByNumber returns canonical block n ("latest" if n < 0), or nil.
func (r *RPC) BlockByNumber(ctx context.Context, n int64) (*Block, error) {
	tag := "latest"
	if n >= 0 {
		tag = hexutil.EncodeUint64(uint64(n))
	}
	var b *Block
	err := r.Call(ctx, &b, "eth_getBlockByNumber", tag, false)
	return b, err
}

// FilterQuery selects logs for GetLogs.
type FilterQuery struct {
	FromBlock string           `json:"fromBlock,omitempty"` // hex number or tag
	ToBlock   string           `json:"toBlock,omitempty"`
	Addresses []common.Address `json:"address,omitempty"`
	Topics    [][]common.Hash  `json:"topics,omitempty"`
}

// GetLogs returns the canonical logs matching q.
func (r *RPC) GetLogs(ctx context.Context, q FilterQuery) ([]*types.Log, error) {
	var logs []*types.Log
	err := r.Call(ctx, &logs, "eth_getLogs", q)
	return logs, err
}

// RevertError is a call or transaction that reverted.
type RevertError struct {
	Reason string
	Data   string
}

func (e *RevertError) Error() string {
	if e.Reason == "" {
		return "execution reverted"
	}
	return "execution reverted: " + e.Reason
}

// ErrReverted is matched by every *RevertError (errors.Is(err, ErrReverted)).
var ErrReverted = errors.New("execution reverted")

func (e *RevertError) Is(target error) bool { return target == ErrReverted }

func asRevert(err error) error {
	var re *Error
	if errors.As(err, &re) && (re.Code == 3 || re.Code == -32015 || bytes.HasPrefix([]byte(re.Message), []byte("execution reverted"))) {
		reason := re.Message
		if len(reason) > len("execution reverted: ") && reason[:len("execution reverted: ")] == "execution reverted: " {
			reason = reason[len("execution reverted: "):]
		} else if reason == "execution reverted" {
			reason = ""
		}
		data, _ := re.Data.(string)
		return &RevertError{Reason: reason, Data: data}
	}
	return err
}
