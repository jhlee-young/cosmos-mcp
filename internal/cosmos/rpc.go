package cosmos

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

type RPCClient struct {
	endpoint *url.URL
	http     *HTTPClient
	nextID   atomic.Int64
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

func NewRPCClient(rawURL string, httpClient *HTTPClient) (*RPCClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return &RPCClient{endpoint: u, http: httpClient}, nil
}

func (c *RPCClient) Endpoint() string { return RedactedURL(c.endpoint.String()) }

func (c *RPCClient) Call(ctx context.Context, method string, params any) (any, error) {
	method = strings.TrimSpace(method)
	if !allowedRPCMethod(method) {
		return nil, NewError(CodeInvalidInput, fmt.Sprintf("RPC method %q is not allowed in read-only mode", method), nil)
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID.Add(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, NewError(CodeInvalidInput, "RPC params are not JSON serializable", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, NewError(CodeInvalidInput, "failed to build RPC request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	var response rpcResponse
	if err := c.http.DoJSON(ctx, req, &response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, NewError(CodeUpstreamError, fmt.Sprintf("RPC error %d: %s", response.Error.Code, response.Error.Message), nil)
	}
	if len(response.Result) == 0 {
		return nil, NewError(CodeUpstreamError, "RPC response did not contain a result", nil)
	}
	var result any
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return nil, NewError(CodeUpstreamError, "RPC result contained invalid JSON", err)
	}
	return result, nil
}

func (c *RPCClient) Status(ctx context.Context) (any, error) {
	return c.Call(ctx, "status", map[string]any{})
}

func (c *RPCClient) Block(ctx context.Context, height string) (any, error) {
	params := map[string]any{}
	if height != "" {
		value, err := strconv.ParseUint(height, 10, 64)
		if err != nil || value == 0 {
			return nil, NewError(CodeInvalidInput, "block height must be a positive decimal string", err)
		}
		params["height"] = height
	}
	return c.Call(ctx, "block", params)
}

func (c *RPCClient) Transaction(ctx context.Context, hash string) (any, error) {
	hash = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(hash), "0x"), "0X")
	if hash == "" || len(hash)%2 != 0 {
		return nil, NewError(CodeInvalidInput, "transaction hash must be a non-empty hexadecimal string", nil)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return nil, NewError(CodeInvalidInput, "transaction hash must be hexadecimal", nil)
	}
	return c.Call(ctx, "tx", map[string]any{"hash": "0x" + strings.ToUpper(hash), "prove": false})
}

func allowedRPCMethod(method string) bool {
	method = strings.TrimSpace(method)
	if method == "" || strings.HasPrefix(method, "unsafe_") || strings.HasPrefix(method, "broadcast_tx_") {
		return false
	}
	switch method {
	case "status", "health", "net_info", "blockchain", "genesis", "genesis_chunked",
		"block", "block_by_hash", "block_results", "commit", "header", "header_by_hash",
		"validators", "dump_consensus_state", "consensus_state", "consensus_params",
		"unconfirmed_txs", "num_unconfirmed_txs", "tx", "tx_search", "abci_query", "check_tx":
		return true
	default:
		return false
	}
}
