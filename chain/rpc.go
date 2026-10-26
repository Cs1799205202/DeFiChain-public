package chain

// Framework file: the Ethereum JSON-RPC server, so that MetaMask, ethers.js, web3.py,
// Foundry (cast/forge) and the course's Go SDK all work against your node unchanged.
// POST / or /rpc; batches are supported. There are no WebSocket subscriptions: clients
// poll (eth_newFilter + eth_getFilterChanges), which every library supports.
//
// Two conventions differ from Ethereum mainnet, because this is a PoW chain without
// finality: "safe" means 6 blocks below the head and "finalized" 12 blocks below it.
// Receipts carry one extra field, "revertReason", for failed transactions.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Confirmation depths used for the "safe" and "finalized" block tags.
const (
	SafeDepth      = 6
	FinalizedDepth = 12
)

// SuggestedTip is what eth_gasPrice / eth_maxPriorityFeePerGas suggest (1 gwei). The base
// fee is always 0, so a transaction costs gasUsed * tip, paid to the miner.
var SuggestedTip = big.NewInt(1_000_000_000)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	ID     json.RawMessage
	Result any
	Error  *rpcError
}

// MarshalJSON writes "result": null for a nil result (JSON-RPC requires the member, and
// clients poll receipts until it is not null), and omits it on error.
func (r rpcResponse) MarshalJSON() ([]byte, error) {
	id := r.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if r.Error != nil {
		return json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *rpcError       `json:"error"`
		}{"2.0", id, r.Error})
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, r.Result})
}

func errParams(format string, args ...any) *rpcError {
	return &rpcError{Code: -32602, Message: fmt.Sprintf(format, args...)}
}

func errServer(err error) *rpcError { return &rpcError{Code: -32000, Message: err.Error()} }

func (n *Node) registerRPC(mux *http.ServeMux) {
	for _, p := range []string{"/{$}", "/rpc", "/rpc/"} {
		mux.HandleFunc("POST "+p, n.handleRPC)
		mux.HandleFunc("OPTIONS "+p, func(w http.ResponseWriter, r *http.Request) {
			cors(w)
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// cors lets web pages (a dapp, MetaMask's page context) call JSON-RPC. The node holds no
// keys, so a page can do nothing through it that it could not do by itself.
func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func (n *Node) handleRPC(w http.ResponseWriter, r *http.Request) {
	cors(w)
	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&raw); err != nil {
		writeJSON(w, http.StatusOK, rpcResponse{Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if trimmed := strings.TrimSpace(string(raw)); strings.HasPrefix(trimmed, "[") {
		var reqs []rpcRequest
		if err := json.Unmarshal(raw, &reqs); err != nil {
			writeJSON(w, http.StatusOK, rpcResponse{Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
			return
		}
		if len(reqs) > 1000 {
			writeJSON(w, http.StatusOK, rpcResponse{Error: &rpcError{Code: -32600, Message: "batch too large"}})
			return
		}
		out := make([]rpcResponse, len(reqs))
		for i, req := range reqs {
			out[i] = n.serveRPC(req)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusOK, rpcResponse{Error: &rpcError{Code: -32600, Message: "invalid request: " + err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, n.serveRPC(req))
}

func (n *Node) serveRPC(req rpcRequest) rpcResponse {
	res, err := n.dispatch(req.Method, req.Params)
	return rpcResponse{ID: req.ID, Result: res, Error: err}
}

// args decodes positional params into dst; params beyond the given ones keep their zero
// value. The first `required` params must be present.
func args(params json.RawMessage, required int, dst ...any) *rpcError {
	var list []json.RawMessage
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &list); err != nil {
			return errParams("params must be an array: %v", err)
		}
	}
	if len(list) < required {
		return errParams("missing value for required argument %d", len(list))
	}
	for i, d := range dst {
		if i >= len(list) {
			break
		}
		if err := json.Unmarshal(list[i], d); err != nil {
			return errParams("invalid argument %d: %v", i, err)
		}
	}
	return nil
}

// blockParam is a block tag, number, or (EIP-1898) {"blockHash": ...} / {"blockNumber": ...}.
type blockParam struct {
	tag  string
	num  *uint64
	hash *common.Hash
}

func (p *blockParam) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		switch s {
		case "latest", "pending", "earliest", "safe", "finalized", "":
			p.tag = s
			return nil
		}
		v, err := hexutil.DecodeUint64(s)
		if err != nil {
			return fmt.Errorf("invalid block number %q", s)
		}
		p.num = &v
		return nil
	}
	var obj struct {
		BlockHash   *common.Hash `json:"blockHash"`
		BlockNumber *blockParam  `json:"blockNumber"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		var v uint64 // some tools send a bare number
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("invalid block parameter %s", data)
		}
		p.num = &v
		return nil
	}
	if obj.BlockHash != nil {
		p.hash = obj.BlockHash
	} else if obj.BlockNumber != nil {
		*p = *obj.BlockNumber
	}
	return nil
}

// resolve returns the block p refers to, or nil if there is no such block.
func (n *Node) resolve(p *blockParam) *types.Block {
	bc := n.Chain
	if p == nil {
		return bc.CurrentBlock()
	}
	switch {
	case p.hash != nil:
		return bc.GetBlock(*p.hash)
	case p.num != nil:
		return bc.GetBlockByNumber(*p.num)
	}
	head := bc.CurrentBlock()
	depth := uint64(0)
	switch p.tag {
	case "earliest":
		return bc.Genesis()
	case "safe":
		depth = SafeDepth
	case "finalized":
		depth = FinalizedDepth
	}
	if head.NumberU64() <= depth {
		return bc.Genesis()
	}
	return bc.GetBlockByNumber(head.NumberU64() - depth)
}

func (n *Node) stateAt(p *blockParam) (*state.StateDB, *types.Block, *rpcError) {
	b := n.resolve(p)
	if b == nil {
		return nil, nil, &rpcError{Code: -32000, Message: "header not found"}
	}
	st, err := n.Chain.StateAt(b)
	if err != nil {
		return nil, nil, errServer(err)
	}
	return st, b, nil
}

func (n *Node) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	bc := n.Chain
	switch method {
	case "web3_clientVersion":
		return "defichain/v1.0", nil
	case "web3_sha3":
		var data hexutil.Bytes
		if err := args(params, 1, &data); err != nil {
			return nil, err
		}
		return hexutil.Bytes(crypto.Keccak256(data)), nil
	case "net_version":
		return fmt.Sprint(bc.Config().ChainID), nil
	case "net_listening":
		return true, nil
	case "net_peerCount":
		return hexutil.Uint64(len(n.Net.Peers())), nil
	case "eth_chainId":
		return hexutil.Uint64(bc.Config().ChainID), nil
	case "eth_syncing":
		return false, nil
	case "eth_mining":
		return n.Miner.Mining(), nil
	case "eth_hashrate":
		return hexutil.Uint64(uint64(n.Throttle.Rate())), nil
	case "eth_coinbase":
		return n.Coinbase, nil
	case "eth_accounts":
		return []common.Address{}, nil // the node holds no keys
	case "eth_sign", "eth_signTransaction", "eth_sendTransaction", "personal_sign", "eth_signTypedData_v4":
		return nil, &rpcError{Code: -32601, Message: "the node holds no keys: sign the transaction locally and use eth_sendRawTransaction"}
	case "eth_blockNumber":
		return hexutil.Uint64(bc.CurrentBlock().NumberU64()), nil
	case "eth_gasPrice", "eth_maxPriorityFeePerGas":
		return (*hexutil.Big)(SuggestedTip), nil
	case "eth_blobBaseFee":
		return (*hexutil.Big)(new(big.Int)), nil
	case "eth_feeHistory":
		return n.rpcFeeHistory(params)
	case "eth_getBalance", "eth_getCode", "eth_getTransactionCount", "eth_getStorageAt":
		return n.rpcAccount(method, params)
	case "eth_call":
		return n.rpcCall(params)
	case "eth_estimateGas":
		return n.rpcEstimateGas(params)
	case "eth_sendRawTransaction":
		return n.rpcSendRaw(params)
	case "eth_getBlockByNumber", "eth_getBlockByHash":
		var p blockParam
		var full bool
		if method == "eth_getBlockByHash" {
			var h common.Hash
			if err := args(params, 1, &h, &full); err != nil {
				return nil, err
			}
			p.hash = &h
		} else if err := args(params, 1, &p, &full); err != nil {
			return nil, err
		}
		b := n.resolve(&p)
		if b == nil {
			return nil, nil
		}
		return n.blockJSON(b, full), nil
	case "eth_getBlockTransactionCountByNumber", "eth_getBlockTransactionCountByHash":
		p, err := n.blockArg(method, params)
		if err != nil {
			return nil, err
		}
		if b := n.resolve(p); b != nil {
			return hexutil.Uint64(len(b.Transactions())), nil
		}
		return nil, nil
	case "eth_getUncleCountByBlockNumber", "eth_getUncleCountByBlockHash":
		return hexutil.Uint64(0), nil
	case "eth_getUncleByBlockNumberAndIndex", "eth_getUncleByBlockHashAndIndex":
		return nil, nil
	case "eth_getTransactionByHash":
		var h common.Hash
		if err := args(params, 1, &h); err != nil {
			return nil, err
		}
		if info := bc.LookupTx(h); info != nil {
			return n.txJSON(info.Tx, info.Block, info.Index), nil
		}
		if tx := n.Pool.Get(h); tx != nil {
			return n.txJSON(tx, nil, 0), nil
		}
		return nil, nil
	case "eth_getTransactionByBlockNumberAndIndex", "eth_getTransactionByBlockHashAndIndex":
		var p blockParam
		var idx hexutil.Uint64
		if method == "eth_getTransactionByBlockHashAndIndex" {
			var h common.Hash
			if err := args(params, 2, &h, &idx); err != nil {
				return nil, err
			}
			p.hash = &h
		} else if err := args(params, 2, &p, &idx); err != nil {
			return nil, err
		}
		b := n.resolve(&p)
		if b == nil || int(idx) >= len(b.Transactions()) {
			return nil, nil
		}
		return n.txJSON(b.Transactions()[idx], b, int(idx)), nil
	case "eth_getTransactionReceipt":
		var h common.Hash
		if err := args(params, 1, &h); err != nil {
			return nil, err
		}
		info := bc.LookupTx(h)
		if info == nil || info.Receipt == nil {
			return nil, nil // pending or unknown: null, clients keep polling
		}
		return n.receiptJSON(info.Tx, info.Block, info.Index, info.Receipt, info.Revert), nil
	case "eth_getBlockReceipts":
		var p blockParam
		if err := args(params, 1, &p); err != nil {
			return nil, err
		}
		b := n.resolve(&p)
		if b == nil {
			return nil, nil
		}
		res := bc.GetResult(b.Hash())
		out := []any{}
		for i, tx := range b.Transactions() {
			if res != nil && i < len(res.Receipts) {
				out = append(out, n.receiptJSON(tx, b, i, res.Receipts[i], res.Reverts[i]))
			}
		}
		return out, nil
	case "eth_getLogs":
		return n.rpcGetLogs(params)
	case "eth_newFilter", "eth_newBlockFilter", "eth_newPendingTransactionFilter",
		"eth_getFilterChanges", "eth_getFilterLogs", "eth_uninstallFilter":
		return n.rpcFilter(method, params)
	case "txpool_status":
		pending, queued := n.Pool.Content()
		count := func(m map[common.Address][]*types.Transaction) (c int) {
			for _, l := range m {
				c += len(l)
			}
			return
		}
		return map[string]hexutil.Uint64{"pending": hexutil.Uint64(count(pending)), "queued": hexutil.Uint64(count(queued))}, nil
	case "txpool_content":
		pending, queued := n.Pool.Content()
		conv := func(m map[common.Address][]*types.Transaction) map[string]map[string]any {
			out := map[string]map[string]any{}
			for addr, txs := range m {
				byNonce := map[string]any{}
				for _, tx := range txs {
					byNonce[fmt.Sprint(tx.Nonce())] = n.txJSON(tx, nil, 0)
				}
				out[addr.Hex()] = byNonce
			}
			return out
		}
		return map[string]any{"pending": conv(pending), "queued": conv(queued)}, nil
	}
	return nil, &rpcError{Code: -32601, Message: fmt.Sprintf("the method %s does not exist/is not available", method)}
}

func (n *Node) blockArg(method string, params json.RawMessage) (*blockParam, *rpcError) {
	var p blockParam
	if strings.HasSuffix(method, "ByHash") {
		var h common.Hash
		if err := args(params, 1, &h); err != nil {
			return nil, err
		}
		p.hash = &h
	} else if err := args(params, 1, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (n *Node) rpcAccount(method string, params json.RawMessage) (any, *rpcError) {
	var addr common.Address
	var p blockParam
	var slot hexutil.Bytes
	var err *rpcError
	if method == "eth_getStorageAt" {
		var key string
		if err = args(params, 2, &addr, &key, &p); err == nil {
			slot = common.FromHex(key)
		}
	} else {
		err = args(params, 1, &addr, &p)
	}
	if err != nil {
		return nil, err
	}
	if method == "eth_getTransactionCount" && p.tag == "pending" {
		return hexutil.Uint64(n.Pool.PendingNonce(addr)), nil
	}
	st, _, err := n.stateAt(&p)
	if err != nil {
		return nil, err
	}
	switch method {
	case "eth_getBalance":
		return (*hexutil.Big)(st.GetBalance(addr).ToBig()), nil
	case "eth_getCode":
		return hexutil.Bytes(st.GetCode(addr)), nil
	case "eth_getTransactionCount":
		return hexutil.Uint64(st.GetNonce(addr)), nil
	}
	return st.GetState(addr, common.BytesToHash(slot)), nil
}

// callArgs is the transaction object of eth_call and eth_estimateGas. Numbers may be hex
// strings (the standard) or plain JSON numbers (what some scripts send).
type callArgs struct {
	From     *common.Address `json:"from"`
	To       *common.Address `json:"to"`
	Gas      *num            `json:"gas"`
	Value    *num            `json:"value"`
	Data     *hexutil.Bytes  `json:"data"`
	Input    *hexutil.Bytes  `json:"input"`
	GasPrice *num            `json:"gasPrice"`
	MaxFee   *num            `json:"maxFeePerGas"`
	MaxTip   *num            `json:"maxPriorityFeePerGas"`
	Nonce    *num            `json:"nonce"`
	ChainID  *num            `json:"chainId"`
	Type     *num            `json:"type"`
	// accessList etc. are accepted and ignored.
	AccessList json.RawMessage `json:"accessList"`
}

// num is a quantity given as a hex string, a decimal string or a JSON number.
type num big.Int

func (n *num) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	v, ok := new(big.Int).SetString(s, 0)
	if !ok || v.Sign() < 0 {
		return fmt.Errorf("invalid quantity %s", data)
	}
	*n = num(*v)
	return nil
}

func (n *num) Int() *big.Int { return (*big.Int)(n) }

func (a *callArgs) msg() CallMsg {
	m := CallMsg{To: a.To}
	if a.From != nil {
		m.From = *a.From
	}
	if a.Gas != nil && a.Gas.Int().IsUint64() {
		m.Gas = a.Gas.Int().Uint64()
	}
	if a.Value != nil {
		m.Value = a.Value.Int()
	}
	if a.Input != nil {
		m.Data = *a.Input
	} else if a.Data != nil {
		m.Data = *a.Data
	}
	return m
}

func revertErr(e *RevertError) *rpcError {
	return &rpcError{Code: 3, Message: e.Error(), Data: hexutil.Bytes(e.Data).String()}
}

func (n *Node) rpcCall(params json.RawMessage) (any, *rpcError) {
	var a callArgs
	var p blockParam
	if err := args(params, 1, &a, &p); err != nil {
		return nil, err
	}
	st, b, rerr := n.stateAt(&p)
	if rerr != nil {
		return nil, rerr
	}
	res, err := n.Chain.Executor().Call(st, b.Header(), n.Chain.GetHashFn(b.Header()), a.msg())
	if err != nil {
		return nil, errServer(err)
	}
	if res.Failed() {
		return nil, revertErr(&RevertError{Reason: revertReason(res), Data: res.Revert()})
	}
	return hexutil.Bytes(res.ReturnData), nil
}

func (n *Node) rpcEstimateGas(params json.RawMessage) (any, *rpcError) {
	var a callArgs
	var p blockParam
	if err := args(params, 1, &a, &p); err != nil {
		return nil, err
	}
	st, b, rerr := n.stateAt(&p)
	if rerr != nil {
		return nil, rerr
	}
	gas, err := n.Chain.Executor().EstimateGas(st, b.Header(), n.Chain.GetHashFn(b.Header()), a.msg())
	if err != nil {
		var re *RevertError
		if errors.As(err, &re) {
			return nil, revertErr(re)
		}
		return nil, errServer(err)
	}
	return hexutil.Uint64(gas), nil
}

func (n *Node) rpcSendRaw(params json.RawMessage) (any, *rpcError) {
	var raw hexutil.Bytes
	if err := args(params, 1, &raw); err != nil {
		return nil, err
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, errParams("invalid transaction: %v", err)
	}
	added, err := n.Pool.Add(tx)
	if err != nil {
		return nil, errServer(err)
	}
	if added {
		n.Net.BroadcastTx(tx, "")
	}
	return tx.Hash(), nil
}

func (n *Node) rpcFeeHistory(params json.RawMessage) (any, *rpcError) {
	var countRaw json.RawMessage
	var newest blockParam
	var percentiles []float64
	if err := args(params, 2, &countRaw, &newest, &percentiles); err != nil {
		return nil, err
	}
	var count uint64
	var hc hexutil.Uint64
	if err := json.Unmarshal(countRaw, &hc); err == nil {
		count = uint64(hc)
	} else if err := json.Unmarshal(countRaw, &count); err != nil {
		return nil, errParams("invalid block count")
	}
	count = min(max(count, 1), 1024)
	last := n.resolve(&newest)
	if last == nil {
		return nil, &rpcError{Code: -32000, Message: "header not found"}
	}
	if count > last.NumberU64()+1 {
		count = last.NumberU64() + 1
	}
	first := last.NumberU64() + 1 - count
	var baseFees []*hexutil.Big
	var ratios []float64
	var rewards [][]*hexutil.Big
	for i := first; i <= last.NumberU64(); i++ {
		b := n.Chain.GetBlockByNumber(i)
		if b == nil {
			break
		}
		baseFees = append(baseFees, (*hexutil.Big)(new(big.Int)))
		ratios = append(ratios, float64(b.GasUsed())/float64(b.GasLimit()))
		if len(percentiles) > 0 {
			rewards = append(rewards, blockRewards(b, n.Chain.GetResult(b.Hash()), percentiles))
		}
	}
	baseFees = append(baseFees, (*hexutil.Big)(new(big.Int))) // next block's base fee
	out := map[string]any{"oldestBlock": hexutil.Uint64(first), "baseFeePerGas": baseFees, "gasUsedRatio": ratios}
	if len(percentiles) > 0 {
		out["reward"] = rewards
	}
	return out, nil
}

// blockRewards are the tips at the given gas-weighted percentiles of a block.
func blockRewards(b *types.Block, res *BlockResult, percentiles []float64) []*hexutil.Big {
	type tip struct {
		tip *big.Int
		gas uint64
	}
	var tips []tip
	for i, tx := range b.Transactions() {
		if res != nil && i < len(res.Receipts) {
			tips = append(tips, tip{tx.EffectiveGasTipValue(b.BaseFee()), res.Receipts[i].GasUsed})
		}
	}
	sort.Slice(tips, func(i, j int) bool { return tips[i].tip.Cmp(tips[j].tip) < 0 })
	out := make([]*hexutil.Big, len(percentiles))
	var cum uint64
	j := 0
	for i, p := range percentiles {
		if len(tips) == 0 {
			out[i] = (*hexutil.Big)(new(big.Int))
			continue
		}
		threshold := uint64(float64(b.GasUsed()) * p / 100)
		for j < len(tips)-1 && cum+tips[j].gas < threshold {
			cum += tips[j].gas
			j++
		}
		out[i] = (*hexutil.Big)(tips[j].tip)
	}
	return out
}

// ---- JSON formats ----------------------------------------------------------------------

func (n *Node) blockJSON(b *types.Block, full bool) map[string]any {
	h := b.Header()
	td := n.Chain.GetTd(b.Hash())
	if td == nil {
		td = new(big.Int)
	}
	txs := make([]any, 0, len(b.Transactions()))
	for i, tx := range b.Transactions() {
		if full {
			txs = append(txs, n.txJSON(tx, b, i))
		} else {
			txs = append(txs, tx.Hash())
		}
	}
	return map[string]any{
		"number": hexutil.Uint64(h.Number.Uint64()), "hash": b.Hash(), "parentHash": h.ParentHash,
		"nonce": h.Nonce, "mixHash": h.MixDigest, "sha3Uncles": h.UncleHash, "logsBloom": h.Bloom,
		"transactionsRoot": h.TxHash, "stateRoot": h.Root, "receiptsRoot": h.ReceiptHash, "miner": h.Coinbase,
		"difficulty": (*hexutil.Big)(h.Difficulty), "totalDifficulty": (*hexutil.Big)(td),
		"extraData": hexutil.Bytes(h.Extra), "size": hexutil.Uint64(b.Size()), "gasLimit": hexutil.Uint64(h.GasLimit),
		"gasUsed": hexutil.Uint64(h.GasUsed), "timestamp": hexutil.Uint64(h.Time),
		"baseFeePerGas": (*hexutil.Big)(new(big.Int)), "transactions": txs, "uncles": []common.Hash{},
	}
}

// txJSON formats a transaction; b is nil for a pending one.
func (n *Node) txJSON(tx *types.Transaction, b *types.Block, index int) map[string]any {
	from, _ := types.Sender(n.Chain.Executor().signer, tx)
	v, r, s := tx.RawSignatureValues()
	out := map[string]any{
		"hash": tx.Hash(), "nonce": hexutil.Uint64(tx.Nonce()), "from": from, "to": tx.To(),
		"value": (*hexutil.Big)(tx.Value()), "gas": hexutil.Uint64(tx.Gas()), "input": hexutil.Bytes(tx.Data()),
		"type": hexutil.Uint64(tx.Type()), "chainId": (*hexutil.Big)(tx.ChainId()),
		"v": (*hexutil.Big)(v), "r": (*hexutil.Big)(r), "s": (*hexutil.Big)(s),
		"gasPrice": (*hexutil.Big)(tx.GasPrice()), "blockHash": nil, "blockNumber": nil, "transactionIndex": nil,
	}
	if tx.Type() != types.LegacyTxType {
		al := tx.AccessList()
		if al == nil {
			al = types.AccessList{}
		}
		out["accessList"] = al
		out["yParity"] = hexutil.Uint64(v.Uint64())
	}
	if tx.Type() == types.DynamicFeeTxType {
		out["maxFeePerGas"] = (*hexutil.Big)(tx.GasFeeCap())
		out["maxPriorityFeePerGas"] = (*hexutil.Big)(tx.GasTipCap())
		// The price actually paid: min(tip, feeCap) + baseFee, and the base fee is 0.
		out["gasPrice"] = (*hexutil.Big)(tx.EffectiveGasTipValue(new(big.Int)))
	}
	if b != nil {
		out["blockHash"], out["blockNumber"], out["transactionIndex"] = b.Hash(), hexutil.Uint64(b.NumberU64()), hexutil.Uint64(index)
	}
	return out
}

func (n *Node) receiptJSON(tx *types.Transaction, b *types.Block, index int, r *types.Receipt, revert string) map[string]any {
	from, _ := types.Sender(n.Chain.Executor().signer, tx)
	logs := r.Logs
	if logs == nil {
		logs = []*types.Log{}
	}
	var contract any
	if r.ContractAddress != (common.Address{}) {
		contract = r.ContractAddress
	}
	out := map[string]any{
		"transactionHash": tx.Hash(), "transactionIndex": hexutil.Uint64(index), "blockHash": b.Hash(),
		"blockNumber": hexutil.Uint64(b.NumberU64()), "from": from, "to": tx.To(),
		"cumulativeGasUsed": hexutil.Uint64(r.CumulativeGasUsed), "gasUsed": hexutil.Uint64(r.GasUsed),
		"effectiveGasPrice": (*hexutil.Big)(tx.EffectiveGasTipValue(new(big.Int))), "contractAddress": contract,
		"logs": logs, "logsBloom": r.Bloom, "status": hexutil.Uint64(r.Status), "type": hexutil.Uint64(tx.Type()),
	}
	if revert != "" {
		out["revertReason"] = revert
	}
	return out
}
