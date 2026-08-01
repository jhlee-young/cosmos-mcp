# cosmos-mcp

A read-only [Model Context Protocol](https://modelcontextprotocol.io/) server for Cosmos SDK chains. It exposes only the tools backed by the RPC, gRPC, and LCD endpoints configured at startup, and is designed to run as a local stdio server in [Hermes Agent](https://github.com/NousResearch/hermes-agent).

## Features

- Official Go MCP SDK with stdio transport
- Any combination of CometBFT RPC, Cosmos gRPC, and LCD/REST endpoints
- Capability-based tool discovery: unconfigured protocols do not expose tools
- Read-only RPC allowlist, LCD GET-only access, and gRPC Query-service enforcement
- Dynamic gRPC calls through server reflection
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
| RPC | `chain_status`, `get_block`, `get_transaction`, `rpc_query` |
| LCD | `get_balances`, `lcd_query` |
| gRPC | `grpc_query` |

`rpc_query` accepts only known read-only CometBFT methods and rejects `broadcast_tx_*`, `unsafe_*`, and unknown methods. `lcd_query` accepts a path on the configured origin and only performs GET requests; query parameters already fixed on the configured LCD URL cannot be overridden by a tool call. `grpc_query` accepts unary services named `Query` plus a small allowlist of Cosmos read services; the node must expose gRPC reflection.

All results include both MCP text content and structured content:

```json
{
  "source": "rpc",
  "request": {"method": "status"},
  "data": {},
  "meta": {"duration_ms": 12}
}
```

Tool failures use `isError` and one of: `invalid_input`, `not_configured`, `endpoint_unavailable`, `upstream_timeout`, `upstream_error`, `response_too_large`, or `grpc_reflection_unavailable`.

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
- Embedded URL credentials and query strings are removed from status output.
- TLS verification is enabled by default.
- Configure endpoints you trust: query responses are external, untrusted tool data.
- `grpc_query` treats any gRPC service named `...Query` as read-only by Cosmos SDK convention; this is not enforced by the gRPC protocol itself, so a chain that violates the convention could expose a mutating method through it.

## License

[MIT](LICENSE)
