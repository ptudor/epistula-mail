# imap Makefile
# IMAP4 server reading the Postgres + blob store written by epistula-database.

BINARY := epistula-imap
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

BUILD_DIR := build

.PHONY: all build build-linux build-linux-arm64 build-darwin build-darwin-arm64 \
        build-freebsd build-all test fmt vet check clean help

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

clean:
	rm -f $(BINARY) golang-epistula-imap
	rm -rf $(BUILD_DIR)

help:
	@echo "Targets: build build-linux build-linux-arm64 build-darwin build-darwin-arm64 build-freebsd build-all test fmt vet check clean"
