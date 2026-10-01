# Epistula root Makefile
# Builds all services for the specified platform

PROJECTS = database \
           imap \
           api \
           llm-worker \
           mcp

.PHONY: all build build-linux build-linux-arm64 build-darwin build-darwin-arm64 build-freebsd \
        test test-integration vet verify-modules clean deps help release-check snapshot \
        update-notices check-notices test-packages

# Default target
all: build

# Build for current platform
build:
	@for dir in $(PROJECTS); do \
		echo "Building $$dir..."; \
		$(MAKE) -C $$dir build || exit 1; \
	done
	@echo "All builds complete."

# Build for Linux amd64
build-linux:
	@for dir in $(PROJECTS); do \
		echo "Building $$dir for linux/amd64..."; \
		$(MAKE) -C $$dir build-linux || exit 1; \
	done
	@echo "All Linux amd64 builds complete."

# Build for Linux arm64
build-linux-arm64:
	@for dir in $(PROJECTS); do \
		echo "Building $$dir for linux/arm64..."; \
		$(MAKE) -C $$dir build-linux-arm64 || exit 1; \
	done
	@echo "All Linux arm64 builds complete."

# Build for macOS amd64
build-darwin:
	@for dir in $(PROJECTS); do \
		echo "Building $$dir for darwin/amd64..."; \
		$(MAKE) -C $$dir build-darwin || exit 1; \
	done
	@echo "All macOS amd64 builds complete."

# Build for macOS arm64 (Apple Silicon)
build-darwin-arm64:
	@for dir in $(PROJECTS); do \
		echo "Building $$dir for darwin/arm64..."; \
		$(MAKE) -C $$dir build-darwin-arm64 || exit 1; \
	done
	@echo "All macOS arm64 builds complete."

# Build for FreeBSD amd64
build-freebsd:
	@for dir in $(PROJECTS); do \
		echo "Building $$dir for freebsd/amd64..."; \
		$(MAKE) -C $$dir build-freebsd || exit 1; \
	done
	@echo "All FreeBSD amd64 builds complete."

# Run tests in all projects
test:
	@for dir in $(PROJECTS); do \
		echo "Testing $$dir..."; \
		$(MAKE) -C $$dir test || exit 1; \
	done
	@echo "All tests complete."

test-integration:
	@test -n "$(MAIL_DATABASE_TEST_PG)" || { echo 'Set MAIL_DATABASE_TEST_PG to a disposable PostgreSQL bootstrap URL.' >&2; exit 1; }
	$(MAKE) test

vet:
	@for dir in $(PROJECTS); do (cd $$dir && go vet ./...) || exit 1; done

verify-modules:
	@for dir in $(PROJECTS); do (cd $$dir && go mod verify) || exit 1; done

GORELEASER ?= goreleaser
GO_LICENSE ?=
NOTICE_ARGS = $(if $(GO_LICENSE),--go-license "$(GO_LICENSE)",)

release-check:
	$(GORELEASER) check

snapshot:
	sh scripts/snapshot.sh $(GORELEASER)

update-notices:
	$(GORELEASER) build --snapshot --clean
	python3 scripts/update-notices.py $(NOTICE_ARGS)

check-notices:
	python3 scripts/update-notices.py --check $(NOTICE_ARGS)

test-packages:
	sh scripts/test-packages.sh

# Clean all build artifacts
clean:
	@for dir in $(PROJECTS); do \
		echo "Cleaning $$dir..."; \
		$(MAKE) -C $$dir clean || true; \
	done
	@echo "All projects cleaned."

# Update dependencies in all projects
deps:
	@for dir in $(PROJECTS); do \
		echo "Updating deps in $$dir..."; \
		(cd $$dir && go mod tidy) || exit 1; \
	done
	@echo "All dependencies updated."

# Show help
help:
	@echo "Epistula Build System"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build              Build all projects for current platform"
	@echo "  build-linux        Build all for Linux amd64"
	@echo "  build-linux-arm64  Build all for Linux arm64"
	@echo "  build-darwin       Build all for macOS amd64"
	@echo "  build-darwin-arm64 Build all for macOS arm64 (Apple Silicon)"
	@echo "  build-freebsd      Build all for FreeBSD amd64"
	@echo "  test               Run tests in all projects"
	@echo "  clean              Remove build artifacts"
	@echo "  deps               Update Go dependencies"
	@echo "  help               Show this help"
	@echo ""
	@echo "Projects:"
	@for dir in $(PROJECTS); do echo "  - $$dir"; done
