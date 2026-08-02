package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioSubprocessListsConfiguredTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "serve", "--rpc-url", "http://127.0.0.1:1")
	command.Env = append(os.Environ(), "COSMOS_MCP_HELPER_PROCESS=1")
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(result.Tools))
	for _, item := range result.Tools {
		got = append(got, item.Name)
	}
	slices.Sort(got)
	want := []string{"chain_status", "endpoint_status", "get_block", "get_transaction", "rpc_query", "search_transactions"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("COSMOS_MCP_HELPER_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			if err := run(os.Args[i+1:]); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}
