.PHONY: build test lint

build:
	go build ./cmd/cosmos-mcp

test:
	go test -race ./...

lint:
	golangci-lint run --timeout=5m ./...
