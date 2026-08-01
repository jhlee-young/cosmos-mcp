## Summary

<!-- What does this PR change, and why? -->

## Related issue

<!-- Closes #123, or "N/A" -->

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Breaking change (existing behavior changes, e.g. a tool's input/output shape)
- [ ] Documentation
- [ ] Chore / refactor / CI

## Security considerations

<!-- This server proxies read access to live Cosmos endpoints for an LLM agent.
     If this PR touches request validation, allowlists (RPC methods, gRPC
     services, LCD paths), URL/redirect handling, or response size limits,
     explain the impact. Otherwise, write "N/A". -->

## Testing

<!-- How did you verify this? e.g. `go test -race ./...`, `cosmos-mcp validate ...`
     against a real node, manual tool calls via an MCP client. -->

- [ ] `go test -race ./...` passes
- [ ] `go vet ./...` / `golangci-lint run` pass
- [ ] Added or updated tests for the change

## Checklist

- [ ] I updated `README.md` if user-facing behavior (flags, env vars, tools) changed
- [ ] I did not add new dependencies without a note on why they're needed
