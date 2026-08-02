package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jhlee-young/cosmos-mcp/internal/config"
	"github.com/jhlee-young/cosmos-mcp/internal/cosmos"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	MCP   *mcp.Server
	rpc   *cosmos.RPCClient
	lcd   *cosmos.LCDClient
	grpc  *cosmos.GRPCClient
	query *cosmos.Resolver
}

type Meta struct {
	DurationMS int64  `json:"duration_ms"`
	Binding    string `json:"binding,omitempty"`
}

type ToolError struct {
	Code    cosmos.ErrorCode `json:"code"`
	Message string           `json:"message"`
	Details map[string]any   `json:"details,omitempty"`
}

type ToolResponse struct {
	Source  string         `json:"source"`
	Request map[string]any `json:"request"`
	Data    any            `json:"data,omitempty"`
	Meta    Meta           `json:"meta"`
	Error   *ToolError     `json:"error,omitempty"`
}

type EndpointState struct {
	Status   string     `json:"status"`
	Endpoint string     `json:"endpoint,omitempty"`
	Error    *ToolError `json:"error,omitempty"`
}

type endpointStatusData struct {
	RPC  EndpointState `json:"rpc"`
	GRPC EndpointState `json:"grpc"`
	LCD  EndpointState `json:"lcd"`
}

type blockInput struct {
	Height string `json:"height,omitempty" jsonschema:"block height as a positive decimal string; omit for latest"`
}

type transactionInput struct {
	Hash string `json:"hash" jsonschema:"transaction hash in hexadecimal form"`
}

type rpcQueryInput struct {
	Method string         `json:"method" jsonschema:"allowed read-only CometBFT JSON-RPC method"`
	Params map[string]any `json:"params,omitempty" jsonschema:"JSON-RPC method parameters"`
}

type balancesInput struct {
	Address    string           `json:"address" jsonschema:"Cosmos bech32 account address"`
	Denom      string           `json:"denom,omitempty" jsonschema:"optional denomination to select one balance"`
	Pagination *paginationInput `json:"pagination,omitempty"`
}

type paginationInput struct {
	Key        string `json:"key,omitempty" jsonschema:"base64 continuation key returned by a previous call"`
	Offset     uint64 `json:"offset,omitempty"`
	Limit      uint64 `json:"limit,omitempty" jsonschema:"page size; defaults to 50 and cannot exceed 200"`
	CountTotal bool   `json:"count_total,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
}

type addressInput struct {
	Address string `json:"address" jsonschema:"Cosmos bech32 account address"`
}
type denomInput struct {
	Denom string `json:"denom" jsonschema:"token denomination"`
}
type validatorInput struct {
	ValidatorAddress string `json:"validator_address" jsonschema:"Cosmos validator operator address"`
}
type validatorsInput struct {
	Status     string           `json:"status,omitempty" jsonschema:"optional staking validator status"`
	Pagination *paginationInput `json:"pagination,omitempty"`
}
type delegationsInput struct {
	DelegatorAddress string           `json:"delegator_address" jsonschema:"Cosmos delegator account address"`
	Pagination       *paginationInput `json:"pagination,omitempty"`
}
type rewardsInput struct {
	DelegatorAddress string `json:"delegator_address"`
	ValidatorAddress string `json:"validator_address,omitempty"`
}
type proposalsInput struct {
	Status     string           `json:"status,omitempty"`
	Voter      string           `json:"voter,omitempty"`
	Depositor  string           `json:"depositor,omitempty"`
	Pagination *paginationInput `json:"pagination,omitempty"`
}
type proposalInput struct {
	ProposalID string `json:"proposal_id" jsonschema:"positive decimal proposal identifier"`
}
type transactionSearchInput struct {
	Events     []string         `json:"events" jsonschema:"CometBFT event filters such as message.sender='cosmos1...'"`
	Order      string           `json:"order,omitempty" jsonschema:"asc or desc"`
	Pagination *paginationInput `json:"pagination,omitempty" jsonschema:"transaction search supports page-aligned offsets; key and reverse are rejected"`
}
type simulationInput struct {
	TxBytes string `json:"tx_bytes" jsonschema:"base64 protobuf transaction bytes"`
}

type lcdQueryInput struct {
	Path  string            `json:"path" jsonschema:"LCD path beginning with one slash; absolute URLs are rejected"`
	Query map[string]string `json:"query,omitempty" jsonschema:"URL query parameters"`
}

type grpcQueryInput struct {
	Method  string         `json:"method" jsonschema:"fully-qualified unary method in /package.Service/Method form"`
	Request map[string]any `json:"request,omitempty" jsonschema:"request message represented as JSON"`
}

func New(cfg config.Config, version string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if version == "" {
		version = "dev"
	}
	result := &Server{}
	httpClient := cosmos.NewHTTPClient(cfg.Timeout, cfg.MaxResponseBytes)
	var err error
	if cfg.RPCURL != "" {
		result.rpc, err = cosmos.NewRPCClient(cfg.RPCURL, httpClient)
		if err != nil {
			return nil, fmt.Errorf("create RPC client: %w", err)
		}
	}
	if cfg.LCDURL != "" {
		result.lcd, err = cosmos.NewLCDClient(cfg.LCDURL, httpClient)
		if err != nil {
			return nil, fmt.Errorf("create LCD client: %w", err)
		}
	}
	if cfg.GRPCTarget != "" {
		result.grpc, err = cosmos.NewGRPCClient(cfg.GRPCTarget, cfg.GRPCInsecure, cfg.Timeout, cfg.MaxResponseBytes)
		if err != nil {
			return nil, fmt.Errorf("create gRPC client: %w", err)
		}
	}
	result.query = cosmos.NewResolver(result.rpc, result.lcd, result.grpc)
	result.MCP = mcp.NewServer(&mcp.Implementation{Name: "cosmos-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: "Read-only access to the configured Cosmos SDK RPC, gRPC, and LCD endpoints. Only tools backed by configured endpoints are exposed.",
		Logger:       logger,
		Capabilities: &mcp.ServerCapabilities{},
	})
	result.registerTools()
	return result, nil
}

func (s *Server) Run(ctx context.Context) error {
	return s.MCP.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) Close() error {
	if s.grpc != nil {
		return s.grpc.Close()
	}
	return nil
}

func (s *Server) registerTools() {
	mcp.AddTool(s.MCP, tool("endpoint_status", "Check which Cosmos endpoints are configured and whether each is currently reachable."), s.endpointStatus)
	if s.rpc != nil || s.grpc != nil || s.lcd != nil {
		mcp.AddTool(s.MCP, tool("chain_status", "Return chain ID, node information, and sync state using an available endpoint."), s.chainStatus)
		mcp.AddTool(s.MCP, tool("get_block", "Get the latest block or a block at a specified height using an available endpoint."), s.getBlock)
		mcp.AddTool(s.MCP, tool("get_transaction", "Get a transaction and its execution result by hexadecimal hash."), s.getTransaction)
	}
	if s.rpc != nil {
		mcp.AddTool(s.MCP, tool("rpc_query", "[RPC] Call an allowlisted read-only CometBFT JSON-RPC method on the configured endpoint."), s.rpcQuery)
	}
	if s.lcd != nil || s.grpc != nil {
		mcp.AddTool(s.MCP, tool("get_account", "Get a Cosmos account and its sequence information."), s.getAccount)
		mcp.AddTool(s.MCP, tool("get_balances", "Get all balances for a Cosmos account or select one denomination."), s.getBalances)
		mcp.AddTool(s.MCP, tool("get_token_info", "Get token supply and denomination metadata."), s.getTokenInfo)
		mcp.AddTool(s.MCP, tool("get_validators", "Get staking validators."), s.getValidators)
		mcp.AddTool(s.MCP, tool("get_validator", "Get one staking validator."), s.getValidator)
		mcp.AddTool(s.MCP, tool("get_delegations", "Get delegations for an account."), s.getDelegations)
		mcp.AddTool(s.MCP, tool("get_unbonding_delegations", "Get unbonding delegations for an account."), s.getUnbondingDelegations)
		mcp.AddTool(s.MCP, tool("get_rewards", "Get delegation rewards for an account."), s.getRewards)
		mcp.AddTool(s.MCP, tool("get_proposals", "Get governance proposals."), s.getProposals)
		mcp.AddTool(s.MCP, tool("get_proposal", "Get one governance proposal."), s.getProposal)
	}
	if s.rpc != nil || s.lcd != nil || s.grpc != nil {
		mcp.AddTool(s.MCP, tool("search_transactions", "Search transactions using CometBFT event filters."), s.searchTransactions)
	}
	if s.grpc != nil {
		mcp.AddTool(s.MCP, tool("simulate_transaction", "Simulate base64 protobuf transaction bytes without broadcasting."), s.simulateTransaction)
	}
	if s.lcd != nil {
		mcp.AddTool(s.MCP, tool("lcd_query", "[LCD] Perform a read-only GET against a relative path on the configured endpoint."), s.lcdQuery)
	}
	if s.grpc != nil {
		mcp.AddTool(s.MCP, tool("grpc_query", "[gRPC] Invoke a reflected unary Cosmos gRPC Query method using a JSON request."), s.grpcQuery)
	}
}

func tool(name, description string) *mcp.Tool {
	falseValue, trueValue := false, true
	return &mcp.Tool{
		Name:        name,
		Title:       toolTitle(name),
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: &falseValue,
			IdempotentHint:  true,
			OpenWorldHint:   &trueValue,
		},
	}
}

func toolTitle(name string) string {
	parts := strings.Split(name, "_")
	for i, part := range parts {
		switch strings.ToLower(part) {
		case "grpc":
			parts[i] = "gRPC"
		case "rpc":
			parts[i] = "RPC"
		case "lcd":
			parts[i] = "LCD"
		default:
			if part != "" {
				parts[i] = strings.ToUpper(part[:1]) + part[1:]
			}
		}
	}
	return strings.Join(parts, " ")
}

func (s *Server) endpointStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	data, _ := s.CheckEndpoints(ctx)
	return response(started, "system", map[string]any{}, data, nil)
}

func (s *Server) CheckEndpoints(ctx context.Context) (map[string]EndpointState, bool) {
	data := endpointStatusData{
		RPC:  EndpointState{Status: "not_configured"},
		GRPC: EndpointState{Status: "not_configured"},
		LCD:  EndpointState{Status: "not_configured"},
	}
	var wg sync.WaitGroup
	if s.rpc != nil {
		data.RPC = EndpointState{Status: "checking", Endpoint: s.rpc.Endpoint()}
		wg.Go(func() {
			_, err := s.rpc.Status(ctx)
			data.RPC = stateFor(s.rpc.Endpoint(), err)
		})
	}
	if s.lcd != nil {
		data.LCD = EndpointState{Status: "checking", Endpoint: s.lcd.Endpoint()}
		wg.Go(func() {
			data.LCD = stateFor(s.lcd.Endpoint(), s.lcd.Status(ctx))
		})
	}
	if s.grpc != nil {
		data.GRPC = EndpointState{Status: "checking", Endpoint: s.grpc.Endpoint()}
		wg.Go(func() {
			data.GRPC = stateFor(s.grpc.Endpoint(), s.grpc.Status(ctx))
		})
	}
	wg.Wait()
	allAvailable := true
	for _, state := range []EndpointState{data.RPC, data.GRPC, data.LCD} {
		if state.Status == "unavailable" {
			allAvailable = false
		}
	}
	return map[string]EndpointState{"rpc": data.RPC, "grpc": data.GRPC, "lcd": data.LCD}, allAvailable
}

func stateFor(endpoint string, err error) EndpointState {
	if err == nil {
		return EndpointState{Status: "available", Endpoint: endpoint}
	}
	code, message := cosmos.ErrorDetails(err)
	return EndpointState{Status: "unavailable", Endpoint: endpoint, Error: &ToolError{Code: code, Message: message}}
}

func (s *Server) chainStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	resolved, err := s.resolveChainStatus(ctx)
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{}, resolved.Data, err)
}

func summarizeChainStatus(data any) any {
	result, ok := data.(map[string]any)
	if !ok {
		return data
	}
	node, _ := result["node_info"].(map[string]any)
	syncInfo, _ := result["sync_info"].(map[string]any)
	return map[string]any{
		"chain_id":          node["network"],
		"node_version":      node["version"],
		"latest_height":     syncInfo["latest_block_height"],
		"latest_block_hash": syncInfo["latest_block_hash"],
		"latest_block_time": syncInfo["latest_block_time"],
		"catching_up":       syncInfo["catching_up"],
		"node_info":         node,
	}
}

func (s *Server) getBlock(ctx context.Context, _ *mcp.CallToolRequest, in blockInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	resolved, err := s.resolveBlock(ctx, in.Height)
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"height": in.Height}, resolved.Data, err)
}

func (s *Server) getTransaction(ctx context.Context, _ *mcp.CallToolRequest, in transactionInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	resolved, err := s.resolveTransaction(ctx, in.Hash)
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"hash": in.Hash}, resolved.Data, err)
}

func (s *Server) rpcQuery(ctx context.Context, _ *mcp.CallToolRequest, in rpcQueryInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	data, err := s.rpc.Call(ctx, in.Method, in.Params)
	return response(started, "rpc", map[string]any{"method": in.Method, "params": in.Params}, data, err)
}

func (s *Server) getBalances(ctx context.Context, _ *mcp.CallToolRequest, in balancesInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	resolved, err := s.resolveBalances(ctx, in)
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"address": in.Address, "denom": in.Denom, "pagination": in.Pagination}, resolved.Data, err)
}

func (s *Server) lcdQuery(ctx context.Context, _ *mcp.CallToolRequest, in lcdQueryInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	data, err := s.lcd.Get(ctx, in.Path, in.Query)
	return response(started, "lcd", map[string]any{"path": in.Path, "query": in.Query}, data, err)
}

func (s *Server) grpcQuery(ctx context.Context, _ *mcp.CallToolRequest, in grpcQueryInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	raw, err := json.Marshal(in.Request)
	if err != nil {
		return response(started, "grpc", map[string]any{"method": in.Method}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "gRPC request must be valid JSON", err))
	}
	data, err := s.grpc.Query(ctx, in.Method, raw)
	return response(started, "grpc", map[string]any{"method": in.Method, "request": in.Request}, data, err)
}

func response(started time.Time, source string, request map[string]any, data any, err error) (*mcp.CallToolResult, ToolResponse, error) {
	return responseBound(started, source, "", request, data, err)
}

func responseBound(started time.Time, source, binding string, request map[string]any, data any, err error) (*mcp.CallToolResult, ToolResponse, error) {
	if source == "" {
		source = "system"
	}
	out := ToolResponse{Source: source, Request: request, Data: data, Meta: Meta{DurationMS: time.Since(started).Milliseconds(), Binding: binding}}
	result := &mcp.CallToolResult{}
	if err != nil {
		code, message := cosmos.ErrorDetails(err)
		out.Error = &ToolError{Code: code, Message: message, Details: cosmos.ErrorDetailsMap(err)}
		out.Data = nil
		result.IsError = true
	}
	if encoded, marshalErr := json.Marshal(out); marshalErr == nil {
		result.Content = []mcp.Content{&mcp.TextContent{Text: string(encoded)}}
	} else {
		result.IsError = true
		result.Content = []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to encode tool response: %v", marshalErr)}}
	}
	return result, out, nil
}
