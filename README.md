# cosmos-mcp

[![Go Version](https://img.shields.io/github/go-mod/go-version/jhlee-young/cosmos-mcp?style=flat-square)](https://github.com/jhlee-young/cosmos-mcp/blob/main/go.mod)
[![CI](https://img.shields.io/github/actions/workflow/status/jhlee-young/cosmos-mcp/ci.yml?branch=main&label=ci&style=flat-square)](https://github.com/jhlee-young/cosmos-mcp/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/jhlee-young/cosmos-mcp?style=flat-square)](https://github.com/jhlee-young/cosmos-mcp/blob/main/LICENSE)

A read-only [Model Context Protocol](https://modelcontextprotocol.io/) server for Cosmos SDK chains. It exposes only the tools backed by the RPC, gRPC, and LCD endpoints configured at startup, and is designed to run as a local stdio server in [Hermes Agent](https://github.com/NousResearch/hermes-agent).

## Features

- Official Go MCP SDK with stdio transport
- Any combination of CometBFT RPC, Cosmos gRPC, and LCD/REST endpoints
- Capability-based tool discovery: unconfigured protocols do not expose tools
- Read-only RPC allowlist, LCD GET-only access, and gRPC Query-service enforcement
- Dynamic gRPC calls through server reflection
- Version-independent high-level queries with lazy capability discovery and LCD fallback
- Timeouts, response-size limits, TLS by default, and redacted endpoint status
- Single binaries for Linux, macOS, and Windows plus a container image

## Install

```bash
go install github.com/jhlee-young/cosmos-mcp/cmd/cosmos-mcp@latest
```

You can also download an archive and checksum from [GitHub Releases](https://github.com/jhlee-young/cosmos-mcp/releases), or run `ghcr.io/jhlee-young/cosmos-mcp:<version>`.

## Usage

At least one endpoint is required. Endpoint reachability is checked at call time, so a temporarily unavailable node does not prevent the MCP server from starting.

```bash
# RPC only
cosmos-mcp serve --rpc-url https://rpc.example.com

# LCD only
cosmos-mcp serve --lcd-url https://lcd.example.com

# Plaintext gRPC only (common for local nodes)
cosmos-mcp serve --grpc-target localhost:9090 --grpc-insecure

# Any combination
cosmos-mcp serve \
  --rpc-url https://rpc.example.com \
  --grpc-target grpc.example.com:443 \
  --lcd-url https://lcd.example.com
```

Validate the configured endpoints without starting MCP:

```bash
cosmos-mcp validate --rpc-url https://rpc.example.com
```

Configuration can also be supplied through environment variables:

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--rpc-url` | `COSMOS_MCP_RPC_URL` | unset |
| `--grpc-target` | `COSMOS_MCP_GRPC_TARGET` | unset |
| `--grpc-insecure` | `COSMOS_MCP_GRPC_INSECURE` | `false` |
| `--lcd-url` | `COSMOS_MCP_LCD_URL` | unset |
| `--timeout` | `COSMOS_MCP_TIMEOUT` | `15s` |
| `--max-response-bytes` | `COSMOS_MCP_MAX_RESPONSE_BYTES` | `4MiB` |

Flags take precedence over environment variables. gRPC uses TLS certificate verification unless `--grpc-insecure` is explicitly set.

## Hermes Agent

Add a server under `mcp_servers` in `~/.hermes/config.yaml`.

RPC-only example:

```yaml
mcp_servers:
  cosmos:
    command: "cosmos-mcp"
    args: ["serve", "--rpc-url", "https://rpc.example.com"]
    supports_parallel_tool_calls: true
```

LCD-only example:

```yaml
mcp_servers:
  cosmos:
    command: "cosmos-mcp"
    args: ["serve", "--lcd-url", "https://lcd.example.com"]
    supports_parallel_tool_calls: true
```

gRPC-only example:

```yaml
mcp_servers:
  cosmos:
    command: "cosmos-mcp"
    args: ["serve", "--grpc-target", "localhost:9090", "--grpc-insecure"]
    supports_parallel_tool_calls: true
```

Combined example using environment variables:

```yaml
mcp_servers:
  cosmos:
    command: "cosmos-mcp"
    args: ["serve"]
    env:
      COSMOS_MCP_RPC_URL: "https://rpc.example.com"
      COSMOS_MCP_GRPC_TARGET: "grpc.example.com:443"
      COSMOS_MCP_LCD_URL: "https://lcd.example.com"
    supports_parallel_tool_calls: true
```

Container example:

```yaml
mcp_servers:
  cosmos:
    command: "docker"
    args:
      - "run"
      - "--rm"
      - "-i"
      - "ghcr.io/jhlee-young/cosmos-mcp:latest"
      - "serve"
      - "--rpc-url"
      - "https://rpc.example.com"
```

Hermes prefixes discovered tools with the MCP server name, such as `mcp_cosmos_chain_status`.

## Tools

`endpoint_status` is always exposed and reports `available`, `unavailable`, or `not_configured` for each protocol.

| Required endpoint | Tools |
| --- | --- |
| Any RPC, LCD, or gRPC | `chain_status`, `get_block`, `get_transaction`, `search_transactions` |
| LCD or gRPC | `get_account`, `get_balances`, `get_token_info`, `get_validators`, `get_validator`, `get_delegations`, `get_unbonding_delegations`, `get_rewards`, `get_proposals`, `get_proposal` |
| RPC | `rpc_query` |
| LCD | `lcd_query` |
| gRPC | `grpc_query`, `simulate_transaction` |

High-level tools resolve the best API lazily at call time instead of relying on a reported Cosmos SDK version. They prefer current gRPC services, fall back to legacy service versions and LCD routes when an API is absent, and cache successful bindings. Temporary endpoint failures, authentication failures, rate limits, and server errors do not trigger a version fallback. gRPC high-level queries require server reflection unless an LCD fallback is configured.

List tools use a default page size of 50 and enforce a maximum of 200. Module queries return a continuation key instead of automatically fetching every page; `search_transactions` uses page-aligned offsets and rejects key or reverse pagination. Normalized high-level results include the original upstream response under `raw`; `meta.binding` reports the selected gRPC method, LCD route, or RPC method.

`rpc_query` accepts only known read-only CometBFT methods and rejects `broadcast_tx_*`, `unsafe_*`, and unknown methods. `lcd_query` accepts a path on the configured origin and only performs GET requests; query parameters already fixed on the configured LCD URL cannot be overridden by a tool call. `grpc_query` accepts unary services named `Query` plus a small allowlist of Cosmos read services; the node must expose gRPC reflection. `simulate_transaction` accepts base64 protobuf transaction bytes and never broadcasts them.

All results include both MCP text content and structured content:

```json
{
  "source": "rpc",
  "request": {"method": "status"},
  "data": {},
  "meta": {
    "duration_ms": 12,
    "binding": "/cosmos.bank.v1beta1.Query/AllBalances"
  }
}
```

`source` is `rpc`, `lcd`, or `grpc` for the endpoint that answered the call, `system` when the response did not require an endpoint (such as a validation error), or `mixed` when a tool combines sub-queries answered by different endpoints (for example `get_token_info` when supply and metadata resolve through different sources).

Tool failures use `isError` and one of: `invalid_input`, `not_configured`, `endpoint_unavailable`, `upstream_timeout`, `upstream_error`, `response_too_large`, `grpc_reflection_unavailable`, or `unsupported_capability`.

## Development

```bash
go test -race ./...
go vet ./...
go build ./cmd/cosmos-mcp
```

Tests use local in-memory and loopback servers and do not depend on a public Cosmos node.

## Security model

- Tools cannot choose a different host from the endpoint fixed at startup.
- This server does not create, sign, store, or broadcast transactions.
- Transaction simulation is read-only and accepts only already encoded transaction bytes.
- Embedded URL credentials and query strings are removed from status output.
- TLS verification is enabled by default.
- Configure endpoints you trust: query responses are external, untrusted tool data.
- `grpc_query` treats any gRPC service named `...Query` as read-only by Cosmos SDK convention; this is not enforced by the gRPC protocol itself, so a chain that violates the convention could expose a mutating method through it.
