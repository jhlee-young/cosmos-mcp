package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jhlee-young/cosmos-mcp/internal/config"
	"github.com/jhlee-young/cosmos-mcp/internal/server"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cosmos-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "validate":
		return runValidate(args[1:])
	case "version", "--version", "-version":
		fmt.Printf("cosmos-mcp %s (commit %s, built %s)\n", version, commit, date)
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runServe(args []string) error {
	cfg, err := config.Parse(args, os.Getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	app, err := server.New(cfg, version, logger)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx)
}

func runValidate(args []string) error {
	cfg, err := config.Parse(args, os.Getenv)
	if err != nil {
		return err
	}
	app, err := server.New(cfg, version, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	states, ok := app.CheckEndpoints(ctx)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(states); err != nil {
		return err
	}
	if !ok {
		return errors.New("one or more configured endpoints are unavailable")
	}
	return nil
}

func usage(out *os.File) {
	_, _ = fmt.Fprintln(out, `Usage:
  cosmos-mcp serve [endpoint flags]
  cosmos-mcp validate [endpoint flags]
  cosmos-mcp version

Endpoint flags (at least one is required):
  --rpc-url URL              CometBFT JSON-RPC endpoint
  --grpc-target HOST:PORT    Cosmos gRPC endpoint
  --grpc-insecure            Use plaintext gRPC instead of TLS
  --lcd-url URL              Cosmos LCD/REST endpoint

Common flags:
  --timeout 15s
  --max-response-bytes 4MiB

The same values may be provided with COSMOS_MCP_* environment variables.`)
}
