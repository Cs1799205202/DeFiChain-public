package client

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// Client is one account on the chain: it signs, sends and waits for its transactions,
// keeps track of its nonce, and reads contracts.
//
//	c := client.NewFromName("alice", "http://127.0.0.1:28001")
//	usd, _ := c.Deploy(ctx, client.TokenContract, "US Dollar", "USD")
//	_, err := c.Transact(ctx, client.TokenContract, usd, "mint", c.Account, client.Ether(100))
//	bal, _ := c.ViewBig(ctx, client.TokenContract, usd, "balanceOf", c.Account)
//
// It is safe for concurrent use; transactions sent concurrently get consecutive nonces.
type Client struct {
	Account common.Address
	RPC     *RPC
	key     *ecdsa.PrivateKey

	// Tip is the priority fee (wei per gas) paid to the miner. Nil: 1 gwei. The base
	// fee is always 0. Miners include higher tips first.
	Tip *big.Int
	// Confirmations: how many blocks must be on top of a transaction's block before
	// Wait returns it. 0 returns as soon as the transaction is in the canonical chain,
	// which a reorg may still undo.
	Confirmations uint64
	// WaitTimeout bounds Wait (and so Transact and Deploy). Default 2 minutes.
	WaitTimeout time.Duration
	// PollEvery is how often Wait polls for the receipt. Default 200 ms.
	PollEvery time.Duration
	// ResubmitAfter: if a transaction has no receipt after this long (lost with a crashed
	// node, or dropped by a reorg), Wait sends it again. Default 15 s.
	ResubmitAfter time.Duration

	mu      sync.Mutex
	chainID *big.Int
	nonce   uint64
	synced  bool
}

// New creates a client for key over the given node URLs.
func New(key *ecdsa.PrivateKey, urls ...string) *Client {
	return &Client{Account: AddressOf(key), RPC: NewRPC(urls...), key: key}
}

// NewFromName is New(KeyFor(name), urls...).
func NewFromName(name string, urls ...string) *Client { return New(KeyFor(name), urls...) }

// Key returns the client's private key.
func (c *Client) Key() *ecdsa.PrivateKey { return c.key }

func (c *Client) tip() *big.Int {
	if c.Tip != nil {
		return c.Tip
	}
	return big.NewInt(1_000_000_000)
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// reserve returns the next nonce, fetching the pending nonce from the node the first time
// (or after a failure made the local count unreliable).
func (c *Client) reserve(ctx context.Context) (uint64, *big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chainID == nil {
		id, err := c.RPC.ChainID(ctx)
		if err != nil {
			return 0, nil, err
		}
		c.chainID = id
	}
	if !c.synced {
		n, err := c.RPC.Nonce(ctx, c.Account, "pending")
		if err != nil {
			return 0, nil, err
		}
		c.nonce, c.synced = n, true
	}
	c.nonce++
	return c.nonce - 1, c.chainID, nil
}

// ResetNonce makes the next transaction ask the node for the nonce again.
func (c *Client) ResetNonce() {
	c.mu.Lock()
	c.synced = false
	c.mu.Unlock()
}

// Tx describes a transaction to send.
type Tx struct {
	To    *common.Address // nil: deploy a contract
	Data  []byte
	Value *big.Int
	Gas   uint64   // 0: estimate (and fail early, with the reason, if it would revert)
	Tip   *big.Int // nil: the client's Tip
}

// Send signs and submits tx and returns it without waiting for it to be mined.
func (c *Client) Send(ctx context.Context, tx Tx) (*types.Transaction, error) {
	gas := tx.Gas
	if gas == 0 {
		args := CallArgs{From: c.Account, To: tx.To, Data: tx.Data}
		if tx.Value != nil {
			args.Value = (*hexutil.Big)(tx.Value)
		}
		est, err := c.RPC.EstimateGas(ctx, args)
		if err != nil {
			return nil, err // a *RevertError if the transaction would revert now
		}
		gas = est
	}
	tip := tx.Tip
	if tip == nil {
		tip = c.tip()
	}
	value := tx.Value
	if value == nil {
		value = new(big.Int)
	}
	for attempt := 0; ; attempt++ {
		nonce, chainID, err := c.reserve(ctx)
		if err != nil {
			return nil, err
		}
		signed, err := types.SignNewTx(c.key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
			ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: tip, Gas: gas, To: tx.To, Value: value, Data: tx.Data,
		})
		if err != nil {
			return nil, err
		}
		err = c.RPC.SendRaw(ctx, signed)
		if err == nil {
			return signed, nil
		}
		c.ResetNonce() // we may have left a gap, or someone else used our nonce
		if attempt == 0 && strings.Contains(err.Error(), "nonce too low") {
			continue
		}
		return nil, fmt.Errorf("send: %w", err)
	}
}

// ErrReplaced: the transaction's nonce was used by another transaction from the same
// account, so it can never be included.
var ErrReplaced = errors.New("transaction replaced: its nonce was used by another transaction")

// Wait polls until tx is in the canonical chain with c.Confirmations blocks on top of it,
// and returns its receipt. If the transaction reverted, it returns the receipt and an
// error wrapping ErrReverted. If tx disappears (a reorg took its block away and it is not
// back yet), Wait keeps waiting and resubmits it after ResubmitAfter.
func (c *Client) Wait(ctx context.Context, tx *types.Transaction) (*Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, orDefault(c.WaitTimeout, 2*time.Minute))
	defer cancel()
	poll, resubmit := orDefault(c.PollEvery, 200*time.Millisecond), orDefault(c.ResubmitAfter, 15*time.Second)
	lastSeen := time.Now()
	for {
		rc, err := c.RPC.Receipt(ctx, tx.Hash())
		if err == nil && rc != nil {
			lastSeen = time.Now()
			ok := c.Confirmations == 0
			if !ok {
				head, err := c.RPC.BlockNumber(ctx)
				ok = err == nil && head >= uint64(rc.BlockNumber)+c.Confirmations
			}
			if ok {
				if !rc.Success() {
					return rc, &RevertError{Reason: rc.RevertReason}
				}
				return rc, nil
			}
		} else if err == nil && time.Since(lastSeen) > resubmit {
			if n, err := c.RPC.Nonce(ctx, c.Account, "latest"); err == nil && n > tx.Nonce() {
				// Our nonce is used, but not by us (no receipt): we lost the race.
				if rc, _ := c.RPC.Receipt(ctx, tx.Hash()); rc == nil {
					return nil, ErrReplaced
				}
			}
			c.RPC.SendRaw(ctx, tx) //nolint:errcheck // best effort; "already known" is fine
			lastSeen = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for %s: %w", tx.Hash().TerminalString(), ctx.Err())
		case <-time.After(poll):
		}
	}
}

// SendWait is Send then Wait.
func (c *Client) SendWait(ctx context.Context, tx Tx) (*Receipt, error) {
	signed, err := c.Send(ctx, tx)
	if err != nil {
		return nil, err
	}
	return c.Wait(ctx, signed)
}

// Deploy creates a contract and returns its address.
func (c *Client) Deploy(ctx context.Context, con Contract, args ...any) (common.Address, error) {
	data, err := con.DeployData(args...)
	if err != nil {
		return common.Address{}, err
	}
	rc, err := c.SendWait(ctx, Tx{Data: data})
	if err != nil {
		return common.Address{}, fmt.Errorf("deploy %s: %w", con.Name, err)
	}
	if rc.ContractAddress == nil {
		return common.Address{}, fmt.Errorf("deploy %s: receipt has no contract address", con.Name)
	}
	return *rc.ContractAddress, nil
}

// Transact calls a contract method in a transaction and waits for its receipt.
func (c *Client) Transact(ctx context.Context, con Contract, addr common.Address, method string, args ...any) (*Receipt, error) {
	data, err := con.Calldata(method, args...)
	if err != nil {
		return nil, err
	}
	rc, err := c.SendWait(ctx, Tx{To: &addr, Data: data})
	if err != nil {
		return rc, fmt.Errorf("%s.%s: %w", con.Name, method, err)
	}
	return rc, nil
}

// SendCall sends a contract call without waiting (use Wait later).
func (c *Client) SendCall(ctx context.Context, con Contract, addr common.Address, method string, args ...any) (*types.Transaction, error) {
	data, err := con.Calldata(method, args...)
	if err != nil {
		return nil, err
	}
	tx, err := c.Send(ctx, Tx{To: &addr, Data: data})
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", con.Name, method, err)
	}
	return tx, nil
}

// View runs a read-only call and returns the decoded outputs.
func (c *Client) View(ctx context.Context, con Contract, addr common.Address, method string, args ...any) ([]any, error) {
	data, err := con.Calldata(method, args...)
	if err != nil {
		return nil, err
	}
	ret, err := c.RPC.EthCall(ctx, CallArgs{From: c.Account, To: &addr, Data: data})
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", con.Name, method, err)
	}
	return con.Decode(method, ret)
}

// ViewBig is View for methods that return a single uint256.
func (c *Client) ViewBig(ctx context.Context, con Contract, addr common.Address, method string, args ...any) (*big.Int, error) {
	out, err := c.View(ctx, con, addr, method, args...)
	if err != nil {
		return nil, err
	}
	if len(out) > 0 {
		if v, ok := out[0].(*big.Int); ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("%s.%s: does not return a uint", con.Name, method)
}

// Balance is the account's ETH balance.
func (c *Client) Balance(ctx context.Context) (*big.Int, error) { return c.RPC.Balance(ctx, c.Account) }

// BlockNumber is the canonical head's number.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) { return c.RPC.BlockNumber(ctx) }

// Ether is n * 10^18 (an amount of ETH, or of an 18-decimal token, in base units).
func Ether(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
