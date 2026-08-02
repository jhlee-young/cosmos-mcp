package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jhlee-young/cosmos-mcp/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolsFollowConfiguredEndpoints(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{name: "rpc", cfg: testConfig("rpc"), want: []string{"chain_status", "endpoint_status", "get_block", "get_transaction", "rpc_query", "search_transactions"}},
		{name: "lcd", cfg: testConfig("lcd"), want: []string{"chain_status", "endpoint_status", "get_account", "get_balances", "get_block", "get_delegations", "get_proposal", "get_proposals", "get_rewards", "get_token_info", "get_transaction", "get_unbonding_delegations", "get_validator", "get_validators", "lcd_query", "search_transactions"}},
		{name: "grpc", cfg: testConfig("grpc"), want: []string{"chain_status", "endpoint_status", "get_account", "get_balances", "get_block", "get_delegations", "get_proposal", "get_proposals", "get_rewards", "get_token_info", "get_transaction", "get_unbonding_delegations", "get_validator", "get_validators", "grpc_query", "search_transactions", "simulate_transaction"}},
		{name: "all", cfg: testConfig("rpc", "lcd", "grpc"), want: []string{"chain_status", "endpoint_status", "get_account", "get_balances", "get_block", "get_delegations", "get_proposal", "get_proposals", "get_rewards", "get_token_info", "get_transaction", "get_unbonding_delegations", "get_validator", "get_validators", "grpc_query", "lcd_query", "rpc_query", "search_transactions", "simulate_transaction"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, err := New(test.cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = app.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverSession, err := app.MCP.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = serverSession.Close() }()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
			clientSession, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = clientSession.Close() }()
			listed, err := clientSession.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(listed.Tools))
			for _, item := range listed.Tools {
				got = append(got, item.Name)
				if item.Annotations == nil || !item.Annotations.ReadOnlyHint || !item.Annotations.IdempotentHint || item.Annotations.DestructiveHint == nil || *item.Annotations.DestructiveHint {
					t.Errorf("tool %s has incorrect annotations: %#v", item.Name, item.Annotations)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("tools = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCheckEndpointsReportsConfiguredAndMissing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"default_node_info": map[string]any{}})
	}))
	cfg := config.Config{RPCURL: upstream.URL, LCDURL: upstream.URL, Timeout: time.Second, MaxResponseBytes: 1 << 20}
	app, err := New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	states, ok := app.CheckEndpoints(context.Background())
	if !ok || states["rpc"].Status != "available" || states["lcd"].Status != "available" || states["grpc"].Status != "not_configured" {
		t.Fatalf("CheckEndpoints() = %#v, %v", states, ok)
	}
	upstream.Close()
	states, ok = app.CheckEndpoints(context.Background())
	if ok || states["rpc"].Status != "unavailable" || states["lcd"].Status != "unavailable" {
		t.Fatalf("CheckEndpoints() after close = %#v, %v", states, ok)
	}
}

func TestToolErrorsAreStructured(t *testing.T) {
	app, err := New(testConfig("rpc"), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := app.MCP.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "rpc_query",
		Arguments: map[string]any{"method": "broadcast_tx_sync", "params": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("CallTool() = %#v, want tool error", result)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content type = %T", result.StructuredContent)
	}
	errorObject, _ := structured["error"].(map[string]any)
	if errorObject["code"] != "invalid_input" {
		t.Fatalf("structured error = %#v", errorObject)
	}
}

func testConfig(protocols ...string) config.Config {
	cfg := config.Config{Timeout: 100 * time.Millisecond, MaxResponseBytes: 1 << 20}
	for _, protocol := range protocols {
		switch protocol {
		case "rpc":
			cfg.RPCURL = "http://127.0.0.1:1"
		case "lcd":
			cfg.LCDURL = "http://127.0.0.1:1"
		case "grpc":
			cfg.GRPCTarget = "127.0.0.1:1"
			cfg.GRPCInsecure = true
		}
	}
	return cfg
}
