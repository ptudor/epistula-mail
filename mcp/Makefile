# mcp Makefile
# MCP server exposing the mail store (via epistula-api's /v1 HTTP contract) to MCP
# clients. A single static binary, cross-compiled into build/ like the other
# Epistula components.

BINARY := epistula-mcp
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

BUILD_DIR := build

.PHONY: all build build-linux build-linux-arm64 build-darwin build-darwin-arm64 \
        build-freebsd build-all test fmt vet check smoke clean help

all: build

build:
	go build $(LDFLAGS) -o $(BINARY) .

build-linux:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-amd64 .

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-arm64 .

build-darwin:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-amd64 .

build-darwin-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-arm64 .

build-freebsd:
	@mkdir -p $(BUILD_DIR)
	GOOS=freebsd GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-freebsd-amd64 .

build-all: build-linux build-linux-arm64 build-darwin build-darwin-arm64 build-freebsd

test:
	go test -race ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

check: fmt vet
	@echo "fmt + vet clean"

# smoke drives an MCP stdio handshake and lists the tools. It needs a config
# (env or file) because the binary builds the epistula-api client at startup — set
# MAIL_MCP_BASE_URL + MAIL_MCP_TOKEN, e.g.
#   MAIL_MCP_BASE_URL=http://127.0.0.1:8784 MAIL_MCP_TOKEN=... make smoke
# The trailing `sleep` holds stdin open briefly so the SDK flushes tools/list
# before EOF.
smoke: build
	@{ printf '%s\n' \
	  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
	  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
	  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1; } | ./$(BINARY)

clean:
	rm -f $(BINARY) golang-epistula-mcp
	rm -rf $(BUILD_DIR)

help:
	@echo "Targets: build build-linux build-linux-arm64 build-darwin build-darwin-arm64 build-freebsd build-all test fmt vet check smoke clean"
