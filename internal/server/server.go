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
	MCP  *mcp.Server
	rpc  *cosmos.RPCClient
	lcd  *cosmos.LCDClient
	grpc *cosmos.GRPCClient
}

type Meta struct {
	DurationMS int64 `json:"duration_ms"`
}

type ToolError struct {
	Code    cosmos.ErrorCode `json:"code"`
	Message string           `json:"message"`
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
	Address string `json:"address" jsonschema:"Cosmos bech32 account address"`
	Denom   string `json:"denom,omitempty" jsonschema:"optional denomination to select one balance"`
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
	if s.rpc != nil {
		mcp.AddTool(s.MCP, tool("chain_status", "[RPC] Return chain ID, latest block information, sync state, and node version."), s.chainStatus)
		mcp.AddTool(s.MCP, tool("get_block", "[RPC] Get the latest CometBFT block or a block at a specified height."), s.getBlock)
		mcp.AddTool(s.MCP, tool("get_transaction", "[RPC] Get a transaction and its execution result by hexadecimal hash."), s.getTransaction)
		mcp.AddTool(s.MCP, tool("rpc_query", "[RPC] Call an allowlisted read-only CometBFT JSON-RPC method on the configured endpoint."), s.rpcQuery)
	}
	if s.lcd != nil {
		mcp.AddTool(s.MCP, tool("get_balances", "[LCD] Get all balances for a Cosmos account or select one denomination."), s.getBalances)
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
	data, err := s.rpc.Status(ctx)
	if err == nil {
		data = summarizeChainStatus(data)
	}
	return response(started, "rpc", map[string]any{"method": "status"}, data, err)
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
	data, err := s.rpc.Block(ctx, in.Height)
	return response(started, "rpc", map[string]any{"method": "block", "height": in.Height}, data, err)
}

func (s *Server) getTransaction(ctx context.Context, _ *mcp.CallToolRequest, in transactionInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	data, err := s.rpc.Transaction(ctx, in.Hash)
	return response(started, "rpc", map[string]any{"method": "tx", "hash": in.Hash}, data, err)
}

func (s *Server) rpcQuery(ctx context.Context, _ *mcp.CallToolRequest, in rpcQueryInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	data, err := s.rpc.Call(ctx, in.Method, in.Params)
	return response(started, "rpc", map[string]any{"method": in.Method, "params": in.Params}, data, err)
}

func (s *Server) getBalances(ctx context.Context, _ *mcp.CallToolRequest, in balancesInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	data, err := s.lcd.Balances(ctx, in.Address, in.Denom)
	return response(started, "lcd", map[string]any{"address": in.Address, "denom": in.Denom}, data, err)
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
	out := ToolResponse{Source: source, Request: request, Data: data, Meta: Meta{DurationMS: time.Since(started).Milliseconds()}}
	result := &mcp.CallToolResult{}
	if err != nil {
		code, message := cosmos.ErrorDetails(err)
		out.Error = &ToolError{Code: code, Message: message}
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
