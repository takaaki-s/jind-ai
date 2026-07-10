.PHONY: build install clean test fmt lint lint-install

VERSION := 0.1.0
BINARY := jin
BUILD_DIR := bin

# Pinned tooling versions. Bump deliberately; both local and CI use the same value.
# Note: golangci-lint-action@v6 in CI requires v1.x. Upgrading to v2.x requires
# bumping golangci-lint-action to v7+ and migrating .golangci.yml (currently absent),
# which is out of scope for incidental changes — bump deliberately.
GOLANGCI_LINT_VERSION := v1.64.8
# `go install` writes to $GOBIN if set, else $GOPATH/bin. Mirror that resolution.
GOLANGCI_LINT_BIN_DIR := $(shell go env GOBIN)
ifeq ($(strip $(GOLANGCI_LINT_BIN_DIR)),)
GOLANGCI_LINT_BIN_DIR := $(shell go env GOPATH)/bin
endif
GOLANGCI_LINT := $(GOLANGCI_LINT_BIN_DIR)/golangci-lint

# ldflags for version injection
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/takaaki-s/jind-ai/internal/version.Version=$(VERSION) \
           -X github.com/takaaki-s/jind-ai/internal/version.Commit=$(COMMIT) \
           -X github.com/takaaki-s/jind-ai/internal/version.Date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/jin

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/jin

clean:
	rm -rf $(BUILD_DIR)

test:
	go test -v ./...

test-short:
	go test -short -v ./...

test-e2e:
	go test -tags e2e -v ./test/e2e/

test-race:
	go test -race ./...

test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@echo "HTML report: go tool cover -html=coverage.out -o coverage.html"

fmt:
	go fmt ./...

lint: lint-install
	$(GOLANGCI_LINT) run ./...

# Install the pinned golangci-lint version into $GOPATH/bin if missing or outdated.
# Bump GOLANGCI_LINT_VERSION above to upgrade; the next `make lint` will reinstall.
lint-install:
	@if ! test -x $(GOLANGCI_LINT) || [ "$$($(GOLANGCI_LINT) version --format short 2>/dev/null)" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi
