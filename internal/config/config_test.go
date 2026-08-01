package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseEndpointCombinations(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "none", wantErr: true},
		{name: "rpc", args: []string{"--rpc-url", "https://rpc.example.com"}},
		{name: "grpc", args: []string{"--grpc-target", "grpc.example.com:443"}},
		{name: "lcd", args: []string{"--lcd-url", "https://lcd.example.com"}},
		{name: "all", args: []string{"--rpc-url", "https://rpc.example.com", "--grpc-target", "localhost:9090", "--grpc-insecure", "--lcd-url", "https://lcd.example.com"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.args, nil)
			if (err != nil) != test.wantErr {
				t.Fatalf("Parse() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestParseFlagOverridesEnvironment(t *testing.T) {
	env := map[string]string{
		"COSMOS_MCP_RPC_URL":            "https://env.example.com",
		"COSMOS_MCP_TIMEOUT":            "30s",
		"COSMOS_MCP_MAX_RESPONSE_BYTES": "8MiB",
	}
	cfg, err := Parse([]string{"--rpc-url", "https://flag.example.com", "--timeout", "2s", "--max-response-bytes", "1MiB"}, func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCURL != "https://flag.example.com" || cfg.Timeout != 2*time.Second || cfg.MaxResponseBytes != 1<<20 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestParseRejectsInvalidValues(t *testing.T) {
	for _, args := range [][]string{
		{"--rpc-url", "ftp://example.com"},
		{"--lcd-url", "example.com"},
		{"--grpc-target", "missing-port"},
		{"--rpc-url", "https://example.com", "--timeout", "0s"},
		{"--rpc-url", "https://example.com", "--max-response-bytes", "nope"},
	} {
		if _, err := Parse(args, nil); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", strings.Join(args, " "))
		}
	}
}
